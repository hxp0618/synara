from __future__ import annotations

import base64
import contextlib
import copy
import dataclasses
import datetime as dt
import hashlib
import io
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest
from typing import Any
from unittest import mock

SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import acceptance_runner as acceptance
import registry_release_gate as registry_gate
import registry_supply_chain as supply
import vault_kms_admission_gate as gate


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
GIT_SHA = "a" * 40
VERSION = "0.5.4"
DIGEST = "sha256:" + "1" * 64
TAG_DRIFT_DIGEST = "sha256:" + "2" * 64
IMAGE_REPOSITORY = "registry.example.test/synara/worker"
PUBLIC_KEY = "-----BEGIN PUBLIC KEY-----\nQUJD\n-----END PUBLIC KEY-----\n"
VAULT_IMAGE_REFERENCE = gate._load_vault_baseline(REPO_ROOT)["imageReference"]
DEFAULT_REGISTRY_ACCESS = {
    "usernameEnvironment": "REGISTRY_USERNAME",
    "passwordEnvironment": "REGISTRY_PASSWORD",
    "caCertEnvironment": "REGISTRY_CA_CERT",
}


def registry_signer_identity() -> dict[str, Any]:
    return {
        "verified": True,
        "displayName": supply.VAULT_TRANSIT_AUTH_METHOD,
        "roleName": gate.DEFAULT_VAULT_APPROLE_NAME,
        "type": "batch",
        "orphan": True,
        "policyCount": 1,
        "policiesSha256": hashlib.sha256(
            json.dumps(
                [gate.VAULT_SIGNER_POLICY_NAME],
                separators=(",", ":"),
                sort_keys=True,
            ).encode("utf-8")
        ).hexdigest(),
    }


def production_configuration() -> supply.SupplyChainConfiguration:
    return supply.load_configuration(REPO_ROOT, signing_policy_profile="production")


def gate_options(
    output_dir: pathlib.Path,
    *,
    report_path: pathlib.Path | None = None,
    admission_mode: str = "verify-existing",
    repo_root: pathlib.Path = REPO_ROOT,
) -> gate.GateOptions:
    return gate.GateOptions(
        repo_root=repo_root,
        output_dir=output_dir,
        kube_context="test-context",
        vault_namespace="synara-kms",
        security_namespace="synara-system",
        admission_test_namespace="synara-admission",
        vault_selector="app.kubernetes.io/name=vault",
        vault_approle_name="synara-worker-release-signer",
        expected_approle_policies=("synara-worker-release-signer",),
        registry_release_gate_report=(report_path or output_dir / "worker-registry-release-gate.json"),
        signed_image_ref=None,
        unsigned_image_ref=f"{IMAGE_REPOSITORY}@{'sha256:' + '3' * 64}",
        wrong_key_image_ref=f"{IMAGE_REPOSITORY}@{'sha256:' + '4' * 64}",
        tag_drift_image_ref=f"{IMAGE_REPOSITORY}:latest",
        admission_mode=admission_mode,
        kubectl_bin="kubectl",
        vault_bin="vault",
        cosign_bin="cosign",
        vault_address_env="VAULT_ADDR",
        vault_token_env="VAULT_TOKEN",
        vault_operator_token_env="VAULT_OPERATOR_TOKEN",
        vault_cacert_env="VAULT_CACERT",
        registry_ca_cert_env="REGISTRY_CA_CERT",
        registry_username_env="REGISTRY_USERNAME",
        registry_password_env="REGISTRY_PASSWORD",
        timeout_seconds=300.0,
    )


def vault_secret_inputs(root: pathlib.Path) -> gate.SecretInputs:
    vault_ca = str(root / "vault-ca.crt")
    return gate.SecretInputs(
        vault_environment={
            "VAULT_ADDR": "https://vault.example.test",
            "VAULT_TOKEN": "vault-signer-token",
            "VAULT_CACERT": vault_ca,
        },
        vault_operator_environment={
            "VAULT_ADDR": "https://vault.example.test",
            "VAULT_TOKEN": "vault-operator-token",
            "VAULT_CACERT": vault_ca,
        },
        registry_ca_path=root / "registry-ca.crt",
        registry_username="registry-user",
        registry_password="registry-password",
        registry_username_env="REGISTRY_USERNAME",
        registry_password_env="REGISTRY_PASSWORD",
        registry_ca_env="REGISTRY_CA_CERT",
        vault_env_names=(
            "VAULT_ADDR",
            "VAULT_TOKEN",
            "VAULT_OPERATOR_TOKEN",
            "VAULT_CACERT",
        ),
    )


def current_release_source_hashes() -> dict[str, str]:
    return supply.checked_in_source_hashes(REPO_ROOT, gate.REGISTRY_RELEASE_SOURCE_PATHS)


def cached_signature_report(
    *,
    image_repository: str = IMAGE_REPOSITORY,
    git_sha: str = GIT_SHA,
    version: str = VERSION,
    run_id: str = "stage3-worker-registry-release-test",
    digest: str = DIGEST,
) -> dict[str, Any]:
    return {
        "slot": "cached",
        "reference": f"{image_repository}@{digest}",
        "digest": digest,
        "verifiedSignatureCount": 1,
        "claimType": supply.COSIGN_CLAIM_TYPE,
        "annotations": {
            "synara.git-sha": git_sha,
            "synara.run-id": run_id,
            "synara.slot": "cached",
            "synara.version": version,
        },
        "transparencyLog": cached_signature_transparency_log(),
    }


def cached_signature_transparency_log() -> dict[str, Any]:
    return {
        "bundlePresent": True,
        "verificationMode": "cosign-online-tlog-verification",
        "bundleMediaType": "application/vnd.dev.sigstore.bundle+json;version=0.3",
        "bundleSha256": "b" * 64,
        "entryCount": 1,
        "entries": [
            {
                "logIndex": 7,
                "integratedTime": 1_720_000_000,
                "inclusionProofPresent": True,
                "inclusionProofHashCount": 1,
                "signedEntryTimestampPresent": True,
                "signedEntryTimestampSha256": "c" * 64,
            }
        ],
        "inclusionProofPresent": True,
        "signedEntryTimestampPresent": True,
    }


def registry_release_gate_report_payload(
    *,
    image_repository: str = IMAGE_REPOSITORY,
    git_sha: str = GIT_SHA,
    version: str = VERSION,
    signing_policy_sha256: str | None = None,
    production_signing_profile_sha256: str | None = None,
    source_hashes: dict[str, str] | None = None,
    signatures: list[dict[str, Any]] | None = None,
    registry_access: dict[str, str] | None = None,
    production_registry_boundary: dict[str, Any] | None = None,
    production_registry_boundary_inputs: dict[str, str] | None = None,
) -> dict[str, Any]:
    configuration = production_configuration()
    profile = configuration.production_signing_profile
    assert profile is not None
    if production_registry_boundary is None or production_registry_boundary_inputs is None:
        raise ValueError("production Registry boundary fixtures are required for valid reports")
    registry_access = dict(registry_access or DEFAULT_REGISTRY_ACCESS)
    run_id = "stage3-worker-registry-release-test"
    return {
        "schemaVersion": registry_gate.SCHEMA_VERSION,
        "runId": run_id,
        "mode": "worker-registry-release-gate",
        "status": "pass",
        "configuration": {
            "imageRepository": image_repository,
            "signingPolicyProfile": "production",
            "registryAccess": registry_access,
            "productionRegistryBoundaryInputs": {
                "registryConfig": production_registry_boundary_inputs["registryConfig"],
                "retentionPolicy": production_registry_boundary_inputs["retentionPolicy"],
            },
            "productionRegistryRuntimeEvidenceInputs": {
                "container": production_registry_boundary_inputs["container"],
                "runtimeConfigPath": production_registry_boundary_inputs["runtimeConfigPath"],
            },
        },
        "source": {
            "gitSha": git_sha,
            "version": version,
            "worktreeDirty": False,
            "sourceHashes": source_hashes or current_release_source_hashes(),
            "supplyChain": {
                **configuration.source_evidence(),
                "signingPolicy": {
                    **configuration.signing_policy.as_report(),
                    "sha256": signing_policy_sha256 or configuration.signing_policy.sha256,
                },
                "productionSigningProfile": {
                    **profile.as_report(),
                    "sha256": production_signing_profile_sha256 or profile.sha256,
                },
            },
        },
        "builds": [
            {
                "slot": "cached",
                "image": f"{image_repository}:stage3-cached",
                "registryDigest": DIGEST,
            }
        ],
        "supplyChain": {
            "status": "pass",
            "signing": {
                "mode": "kms-key",
                "transparencyLogVerified": True,
                "transparencyLogInclusionProofPresent": True,
                "transparencyLogSignedEntryTimestampPresent": True,
                "productionSigningPolicySatisfied": True,
                "signerIdentity": registry_signer_identity(),
                "signatures": signatures
                or [cached_signature_report(image_repository=image_repository, git_sha=git_sha, version=version, run_id=run_id)],
            },
        },
        "security": {
            "outputSecretScan": {
                "status": "pass",
                "findings": [],
            }
        },
        "runtime": {
            "productionRegistryBoundary": copy.deepcopy(production_registry_boundary),
        },
    }


def release_evidence(
    *,
    git_sha: str = GIT_SHA,
    version: str = VERSION,
    signing_policy_sha256: str | None = None,
    production_signing_profile_sha256: str | None = None,
    source_hashes: dict[str, str] | None = None,
    registry_access: dict[str, str] | None = None,
    production_registry_boundary: dict[str, Any] | None = None,
) -> gate.RegistryReleaseEvidence:
    configuration = production_configuration()
    profile = configuration.production_signing_profile
    assert profile is not None
    return gate.RegistryReleaseEvidence(
        run_id="stage3-worker-registry-release-test",
        report_sha256="f" * 64,
        git_sha=git_sha,
        version=version,
        image_repository=IMAGE_REPOSITORY,
        cached_signed_image=f"{IMAGE_REPOSITORY}@{DIGEST}",
        signing_policy_sha256=signing_policy_sha256 or configuration.signing_policy.sha256,
        production_signing_profile_sha256=(
            production_signing_profile_sha256 or profile.sha256
        ),
        source_hashes=dict(source_hashes or current_release_source_hashes()),
        transparency_log_verified=True,
        transparency_log_inclusion_proof_present=True,
        transparency_log_signed_entry_timestamp_present=True,
        cached_signature_transparency_log=cached_signature_transparency_log(),
        registry_access=dict(registry_access or DEFAULT_REGISTRY_ACCESS),
        production_registry_boundary=copy.deepcopy(production_registry_boundary or {}),
        signer_identity={
            **registry_signer_identity(),
            "identitySha256": gate.sha256_text(
                gate._stringify_json(registry_signer_identity())
            ),
        },
    )


def production_registry_boundary_bundle(
    root: pathlib.Path,
    *,
    image_repository: str = IMAGE_REPOSITORY,
    registry_authority: str | None = None,
    tls_peer_certificate_sha256: str = "e" * 64,
    collected_at: str | None = None,
    runtime_config_path: str = "/etc/distribution/config.yml",
    runtime_container: str = "synara-production-registry",
) -> tuple[dict[str, Any], dict[str, str]]:
    checked_in_policy_payload = json.loads(
        (
            REPO_ROOT / gate.registry_gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH
        ).read_text(encoding="utf-8")
    )
    normalized_policy = gate.registry_gate._normalize_retention_policy(
        checked_in_policy_payload,
        code="release.registry_production_boundary_invalid",
    )
    registry_host = image_repository.split("/", 1)[0]
    registry_authority = registry_authority or f"https://{registry_host}"
    registry_config_path = root / "registry-config.yml"
    retention_policy_path = root / "retention-policy.json"
    registry_config_text = (
        "storage:\n"
        "  delete:\n"
        "    enabled: false\n"
        "http:\n"
        f"  host: {registry_authority}\n"
        "  tls:\n"
        "    certificate: /certs/tls.crt\n"
    )
    registry_config_path.write_text(registry_config_text, encoding="utf-8")
    retention_policy_path.write_text(json.dumps(normalized_policy), encoding="utf-8")
    normalized_config_text = gate.registry_gate._normalize_text_content(registry_config_text)
    exported_config_sha256 = hashlib.sha256(
        normalized_config_text.encode("utf-8")
    ).hexdigest()
    retention_policy_sha256 = gate.registry_gate._stable_json_sha256(normalized_policy)
    collected_at = collected_at or dt.datetime.now(dt.timezone.utc).isoformat()
    live_runtime_evidence = {
        "collectedAt": collected_at,
        "registryHost": registry_host,
        "registryAuthority": registry_authority,
        "repositoryAuthority": image_repository,
        "repositoryProbeStatus": 404,
        "tlsPeerCertificateSha256": tls_peer_certificate_sha256,
        "runtimeConfigPath": runtime_config_path,
        "runtimeConfigSha256": exported_config_sha256,
        "exportedConfigSha256": exported_config_sha256,
        "retentionPolicySha256": retention_policy_sha256,
        "checkedInRetentionPolicySha256": retention_policy_sha256,
        "container": {
            "name": runtime_container,
            "id": "a" * 64,
            "image": {
                "expectedReference": registry_gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE,
                "expectedDigest": registry_gate._production_registry_runtime_image_contract(
                    registry_gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE
                )["digest"],
                "configReference": registry_gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE,
                "runtimeId": "sha256:" + "b" * 64,
                "matchedRepoDigest": registry_gate._production_registry_runtime_image_contract(
                    registry_gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE
                )["repoDigest"],
            },
            "startedAt": dt.datetime.now(dt.timezone.utc).isoformat(),
        },
    }
    live_runtime_evidence["runtimeEvidenceSha256"] = gate.registry_gate._stable_json_sha256(
        live_runtime_evidence
    )
    return (
        {
            "registryConfigPath": str(registry_config_path),
            "retentionPolicyPath": str(retention_policy_path),
            "deleteEnabled": False,
            "promotionBoundary": "digest-only",
            "releaseEvidenceDays": normalized_policy["retention"]["releaseEvidenceDays"],
            "garbageCollectionMode": normalized_policy["garbageCollection"]["mode"],
            "archiveRequiredBeforeGc": normalized_policy["garbageCollection"][
                "requiresReleaseEvidenceArchive"
            ],
            "liveRuntimeEvidence": live_runtime_evidence,
        },
        {
            "registryConfig": str(registry_config_path),
            "retentionPolicy": str(retention_policy_path),
            "container": runtime_container,
            "runtimeConfigPath": runtime_config_path,
        },
    )


def kyverno_runtime() -> dict[str, Any]:
    return {
        "deploymentCount": 2,
        "availableDeploymentCount": 2,
        "deployments": [
            "kyverno/kyverno-admission-controller",
            "kyverno/kyverno-background-controller",
        ],
        "verifyImageControllers": [
            {
                "namespace": "kyverno",
                "deployment": "kyverno-admission-controller",
                "serviceAccount": "kyverno-admission-controller",
                "component": "admission-controller",
            },
            {
                "namespace": "kyverno",
                "deployment": "kyverno-background-controller",
                "serviceAccount": "kyverno-background-controller",
                "component": "background-controller",
            },
        ],
    }


def vault_kubectl_side_effect() -> list[dict[str, Any]]:
    audit_observability_data = gate._literal_configmap_data(
        (REPO_ROOT / gate.VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_PATH).read_text(encoding="utf-8"),
        code="test.audit_observability_invalid",
    )
    sidecar_security_context = gate._expected_vault_audit_sidecar_security_context()
    listener_config = """
ui = false

listener "tcp" {
  tls_disable = 0
  address = "[::]:8200"
  cluster_address = "[::]:8201"
  tls_cert_file = "/vault/tls/tls.crt"
  tls_key_file = "/vault/tls/tls.key"
  tls_client_ca_file = "/vault/tls/ca.crt"
}

storage "raft" {
  path = "/vault/data"

  retry_join {
    leader_api_addr = "https://synara-vault-0.synara-vault-internal:8200"
    leader_ca_cert_file = "/vault/tls/ca.crt"
  }

  retry_join {
    leader_api_addr = "https://synara-vault-1.synara-vault-internal:8200"
    leader_ca_cert_file = "/vault/tls/ca.crt"
  }

  retry_join {
    leader_api_addr = "https://synara-vault-2.synara-vault-internal:8200"
    leader_ca_cert_file = "/vault/tls/ca.crt"
  }
}
""".strip()
    return [
        {
            "items": [
                {
                    "metadata": {"name": f"vault-{index}"},
                    "spec": {"nodeName": f"node-{index}"},
                    "status": {
                        "phase": "Running",
                        "conditions": [{"type": "Ready", "status": "True"}],
                    },
                }
                for index in range(3)
            ]
        },
        {
            "items": [
                *[
                    {"metadata": {"name": f"data-synara-vault-{index}"}, "status": {"phase": "Bound"}}
                    for index in range(3)
                ],
                *[
                    {"metadata": {"name": f"audit-synara-vault-{index}"}, "status": {"phase": "Bound"}}
                    for index in range(3)
                ],
            ]
        },
        {
            "items": [
                {
                    "metadata": {"name": "synara-vault"},
                    "spec": {
                        "replicas": 3,
                        "updateStrategy": {"type": "OnDelete"},
                        "template": {
                            "metadata": {"annotations": {}},
                            "spec": {
                                "terminationGracePeriodSeconds": 30,
                                "shareProcessNamespace": True,
                                "volumes": [
                                    {"name": "tls", "secret": {"secretName": "synara-vault-server-tls"}},
                                    {
                                        "name": gate.VAULT_AUDIT_OBSERVABILITY_VOLUME_NAME,
                                        "configMap": {
                                            "name": gate.VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_NAME
                                        },
                                    },
                                    {
                                        "name": gate.VAULT_AUDIT_SIEM_TLS_VOLUME_NAME,
                                        "emptyDir": {"medium": "Memory", "sizeLimit": "4Mi"},
                                    },
                                ],
                                "containers": [
                                    {
                                        "name": "vault",
                                        "image": VAULT_IMAGE_REFERENCE,
                                        "env": [
                                            {"name": "VAULT_LOCAL_CONFIG", "value": listener_config},
                                            {
                                                "name": "VAULT_CACERT",
                                                "value": "/vault/tls/ca.crt",
                                            },
                                        ],
                                        "livenessProbe": {
                                            "exec": {
                                                "command": [
                                                    "/bin/sh",
                                                    "-c",
                                                    "vault status >/dev/null 2>&1; status=$?; "
                                                    '[ "$status" -eq 0 ] || [ "$status" -eq 2 ]',
                                                ]
                                            },
                                            "failureThreshold": 3,
                                            "initialDelaySeconds": 300,
                                            "periodSeconds": 10,
                                            "successThreshold": 1,
                                            "timeoutSeconds": 3,
                                        },
                                        "readinessProbe": {
                                            "exec": {
                                                "command": [
                                                    "/bin/sh",
                                                    "-ec",
                                                    "vault status >/dev/null",
                                                ]
                                            },
                                            "failureThreshold": 3,
                                            "initialDelaySeconds": 300,
                                            "periodSeconds": 10,
                                            "successThreshold": 1,
                                            "timeoutSeconds": 3,
                                        },
                                        "volumeMounts": [
                                            {
                                                "name": "tls",
                                                "mountPath": "/vault/tls",
                                                "readOnly": True,
                                            },
                                            {
                                                "name": "audit",
                                                "mountPath": gate.VAULT_AUDIT_MOUNT_PATH,
                                            },
                                        ],
                                    },
                                    {
                                        "name": gate.VAULT_AUDIT_SHIPPER_NAME,
                                        "image": gate.VAULT_AUDIT_SHIPPER_IMAGE_REFERENCE,
                                        "command": [
                                            "/bin/sh",
                                            f"{gate.VAULT_AUDIT_CONFIG_MOUNT_PATH}/{gate.VAULT_AUDIT_SHIPPER_SCRIPT_KEY}",
                                        ],
                                        "env": [
                                            {
                                                "name": env_name,
                                                "valueFrom": {
                                                    "secretKeyRef": {
                                                        "name": gate.VAULT_AUDIT_SIEM_SECRET_NAME,
                                                        "key": secret_key,
                                                    }
                                                },
                                            }
                                            for env_name, secret_key in gate.VAULT_AUDIT_SIEM_ENVIRONMENT
                                        ],
                                        "securityContext": copy.deepcopy(sidecar_security_context),
                                        "volumeMounts": [
                                            {
                                                "name": "audit",
                                                "mountPath": gate.VAULT_AUDIT_MOUNT_PATH,
                                            },
                                            {
                                                "name": gate.VAULT_AUDIT_OBSERVABILITY_VOLUME_NAME,
                                                "mountPath": gate.VAULT_AUDIT_CONFIG_MOUNT_PATH,
                                                "readOnly": True,
                                            },
                                            {
                                                "name": gate.VAULT_AUDIT_SIEM_TLS_VOLUME_NAME,
                                                "mountPath": gate.VAULT_AUDIT_SIEM_TLS_MOUNT_PATH,
                                            },
                                            {
                                                "name": "tmp",
                                                "mountPath": "/tmp",
                                            },
                                        ],
                                    },
                                    {
                                        "name": gate.VAULT_AUDIT_ROTATION_NAME,
                                        "image": gate.VAULT_AUDIT_ROTATION_IMAGE_REFERENCE,
                                        "command": [
                                            "/bin/sh",
                                            f"{gate.VAULT_AUDIT_CONFIG_MOUNT_PATH}/{gate.VAULT_AUDIT_ROTATION_SCRIPT_KEY}",
                                        ],
                                        "securityContext": copy.deepcopy(sidecar_security_context),
                                        "volumeMounts": [
                                            {
                                                "name": "audit",
                                                "mountPath": gate.VAULT_AUDIT_MOUNT_PATH,
                                            },
                                            {
                                                "name": gate.VAULT_AUDIT_OBSERVABILITY_VOLUME_NAME,
                                                "mountPath": gate.VAULT_AUDIT_CONFIG_MOUNT_PATH,
                                                "readOnly": True,
                                            },
                                            {
                                                "name": "tmp",
                                                "mountPath": "/tmp",
                                            },
                                        ],
                                    },
                                ],
                            },
                        },
                        "volumeClaimTemplates": [
                            {"metadata": {"name": "data"}},
                            {"metadata": {"name": "audit"}},
                        ],
                    },
                }
            ]
        },
        {
            "items": [
                {
                    "metadata": {"name": "synara-vault-server"},
                    "spec": {
                        "minAvailable": 2,
                        "selector": {"matchLabels": dict(gate.VAULT_SERVER_LABELS)},
                    },
                }
            ]
        },
        {
            "metadata": {"name": "synara-vault-server-tls"},
            "type": "kubernetes.io/tls",
            "data": {
                "tls.crt": "dGxz",
                "tls.key": "a2V5",
                "ca.crt": "Y2E=",
            },
        },
        {
            "metadata": {"name": gate.VAULT_NETWORK_POLICY_NAME},
            "spec": {
                "podSelector": {"matchLabels": dict(gate.VAULT_RELEASE_LABELS)},
                "policyTypes": ["Ingress", "Egress"],
                "ingress": [
                    {
                        "from": [
                            {"podSelector": {"matchLabels": dict(gate.VAULT_SERVER_LABELS)}}
                        ],
                        "ports": [
                            {"port": 8200, "protocol": "TCP"},
                            {"port": 8201, "protocol": "TCP"},
                        ],
                    },
                    {
                        "from": [
                            {
                                "namespaceSelector": {
                                    "matchLabels": {
                                        "kubernetes.io/metadata.name": "synara-system"
                                    }
                                }
                            }
                        ],
                        "ports": [{"port": 8200, "protocol": "TCP"}],
                    },
                ],
                "egress": [
                    {
                        "to": [
                            {"podSelector": {"matchLabels": dict(gate.VAULT_SERVER_LABELS)}}
                        ],
                        "ports": [
                            {"port": 8200, "protocol": "TCP"},
                            {"port": 8201, "protocol": "TCP"},
                        ],
                    },
                    {
                        "to": [
                            {
                                "namespaceSelector": {
                                    "matchLabels": dict(gate.VAULT_DNS_NAMESPACE_LABELS)
                                },
                                "podSelector": {
                                    "matchLabels": dict(gate.VAULT_DNS_POD_LABELS)
                                },
                            }
                        ],
                        "ports": [
                            {"port": 53, "protocol": "UDP"},
                            {"port": 53, "protocol": "TCP"},
                        ],
                    },
                    {
                        "to": [
                            {"ipBlock": {"cidr": gate.VAULT_KUBERNETES_SERVICE_IP_CIDR}}
                        ],
                        "ports": [
                            {"port": 443, "protocol": "TCP"},
                        ],
                    },
                    {
                        "to": [
                            {"ipBlock": {"cidr": gate.VAULT_KUBERNETES_APISERVER_IP_CIDR}}
                        ],
                        "ports": [
                            {"port": 6443, "protocol": "TCP"},
                        ],
                    },
                    {
                        "to": [
                            {"ipBlock": {"cidr": gate.VAULT_AUDIT_SIEM_EGRESS_CIDR}}
                        ],
                        "ports": [
                            {"port": gate.VAULT_AUDIT_SIEM_EGRESS_PORT, "protocol": "TCP"},
                        ],
                    },
                ],
            },
        },
        {
            "metadata": {"name": gate.VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_NAME},
            "data": audit_observability_data,
        },
        {
            "metadata": {"name": gate.VAULT_CONFIGMAP_NAME},
            "data": {"extraconfig-from-values.hcl": listener_config},
        },
    ]


def vault_approle_payload(
    *,
    policy_name: str,
    constraints: dict[str, int],
) -> dict[str, Any]:
    return {
        "data": {
            "bind_secret_id": True,
            "token_type": "batch",
            "token_no_default_policy": True,
            "token_policies": [policy_name],
            **constraints,
        }
    }


def vault_token_identity_payload(
    *,
    role_name: str,
    policy_name: str,
    creation_ttl: int,
) -> dict[str, Any]:
    return {
        "data": {
            "display_name": supply.VAULT_TRANSIT_AUTH_METHOD,
            "meta": {"role_name": role_name},
            "type": "batch",
            "orphan": True,
            "path": "auth/approle/login",
            "policies": [policy_name],
            "num_uses": 0,
            "creation_ttl": creation_ttl,
            "ttl": creation_ttl - 1,
        }
    }


def vault_json_side_effect() -> list[dict[str, Any]]:
    return [
        {
            "initialized": True,
            "sealed": False,
            "ha_enabled": True,
            "storage_type": "raft",
        },
        vault_token_identity_payload(
            role_name=gate.DEFAULT_VAULT_OPERATOR_APPROLE_NAME,
            policy_name=gate.VAULT_OPERATOR_POLICY_NAME,
            creation_ttl=gate.OPERATOR_ROLE_CONSTRAINTS["token_ttl"],
        ),
        {"data": {"config": {"servers": [{}, {}, {}]}}},
        {
            "data": {
                "type": "ecdsa-p256",
                "exportable": False,
                "allow_plaintext_backup": False,
                "deletion_allowed": False,
                "derived": False,
                "supports_signing": True,
                "supports_encryption": False,
                "supports_decryption": False,
                "auto_rotate_period": 0,
                "latest_version": 1,
                "keys": {"1": {"public_key": "fixture-public-key"}},
            }
        },
        vault_approle_payload(
            policy_name=gate.VAULT_SIGNER_POLICY_NAME,
            constraints=gate.SIGNER_ROLE_CONSTRAINTS,
        ),
        vault_approle_payload(
            policy_name=gate.VAULT_OPERATOR_POLICY_NAME,
            constraints=gate.OPERATOR_ROLE_CONSTRAINTS,
        ),
        {
            f"{gate.VAULT_AUDIT_DEVICE_PRIMARY_NAME}/": {
                "type": "file",
                "options": {"file_path": gate.VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH},
            },
            f"{gate.VAULT_AUDIT_DEVICE_SECONDARY_NAME}/": {
                "type": "file",
                "options": {"file_path": gate.VAULT_AUDIT_DEVICE_SECONDARY_FILE_PATH},
            },
        },
        vault_token_identity_payload(
            role_name=gate.DEFAULT_VAULT_APPROLE_NAME,
            policy_name=gate.VAULT_SIGNER_POLICY_NAME,
            creation_ttl=gate.SIGNER_ROLE_CONSTRAINTS["token_ttl"],
        ),
        {"request_id": "request-1"},
    ]


class ParseArgsTest(unittest.TestCase):
    def test_accepts_standard_executable_names_and_paths(self) -> None:
        self.assertEqual(gate.normalize_executable("kubectl", "--kubectl-bin"), "kubectl")
        self.assertEqual(gate.normalize_executable("vault", "--vault-bin"), "vault")
        self.assertEqual(gate.normalize_executable("cosign", "--cosign-bin"), "cosign")
        self.assertEqual(
            gate.normalize_executable("/opt/homebrew/bin/cosign", "--cosign-bin"),
            "/opt/homebrew/bin/cosign",
        )

    def test_rejects_executable_control_characters(self) -> None:
        for invalid in ("kubectl\r", "kubectl\n", "kubectl\t", "kubectl\x00"):
            with self.subTest(invalid=repr(invalid)), self.assertRaises(ValueError):
                gate.normalize_executable(invalid, "--kubectl-bin")

    def test_rejects_duplicate_expected_approle_policies(self) -> None:
        with (
            tempfile.TemporaryDirectory() as directory,
            contextlib.redirect_stderr(io.StringIO()),
            self.assertRaises(SystemExit) as caught,
        ):
            gate.parse_args(
                [
                    "--kube-context",
                    "test-context",
                    "--vault-namespace",
                    "vault-system",
                    "--security-namespace",
                    "synara-system",
                    "--admission-test-namespace",
                    "synara-admission",
                    "--expected-approle-policy",
                    "default",
                    "--expected-approle-policy",
                    "default",
                    "--registry-release-gate-report",
                    str(pathlib.Path(directory) / "release.json"),
                    "--unsigned-image-ref",
                    f"{IMAGE_REPOSITORY}@{'sha256:' + '3' * 64}",
                    "--wrong-key-image-ref",
                    f"{IMAGE_REPOSITORY}@{'sha256:' + '4' * 64}",
                    "--tag-drift-image-ref",
                    f"{IMAGE_REPOSITORY}:latest",
                ]
            )
        self.assertEqual(caught.exception.code, 2)


class RegistryReleaseEvidenceTest(unittest.TestCase):
    def test_accepts_passing_production_release_report(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            production_registry_boundary, production_registry_boundary_inputs = (
                production_registry_boundary_bundle(root)
            )
            report_path = root / "release.json"
            report_path.write_text(
                json.dumps(
                    registry_release_gate_report_payload(
                        production_registry_boundary=production_registry_boundary,
                        production_registry_boundary_inputs=production_registry_boundary_inputs,
                    )
                ),
                encoding="utf-8",
            )

            evidence = gate.load_registry_release_evidence(report_path)

        self.assertEqual(evidence.git_sha, GIT_SHA)
        self.assertEqual(evidence.version, VERSION)
        self.assertEqual(evidence.cached_signed_image, f"{IMAGE_REPOSITORY}@{DIGEST}")
        self.assertEqual(
            set(evidence.source_hashes),
            {str(path) for path in gate.REGISTRY_RELEASE_SOURCE_PATHS},
        )
        self.assertEqual(
            evidence.cached_signature_annotations,
            cached_signature_report()["annotations"],
        )
        self.assertTrue(evidence.transparency_log_verified)
        self.assertTrue(evidence.transparency_log_inclusion_proof_present)
        self.assertTrue(evidence.transparency_log_signed_entry_timestamp_present)
        self.assertEqual(evidence.registry_access, DEFAULT_REGISTRY_ACCESS)
        self.assertEqual(
            evidence.production_registry_boundary["liveRuntimeEvidence"]["repositoryAuthority"],
            IMAGE_REPOSITORY,
        )
        self.assertEqual(
            evidence.cached_signature_transparency_log["entryCount"],
            1,
        )
        self.assertEqual(evidence.signer_identity["roleName"], gate.DEFAULT_VAULT_APPROLE_NAME)

    def test_rejects_missing_or_tampered_registry_signer_identity(self) -> None:
        cases: dict[str, tuple[str, Any] | None] = {
            "missing": None,
            "role": ("roleName", "root"),
            "type": ("type", "service"),
            "orphan": ("orphan", False),
            "policy-hash": ("policiesSha256", "0" * 64),
        }
        for case, mutation in cases.items():
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                production_registry_boundary, production_registry_boundary_inputs = (
                    production_registry_boundary_bundle(root)
                )
                payload = registry_release_gate_report_payload(
                    production_registry_boundary=production_registry_boundary,
                    production_registry_boundary_inputs=production_registry_boundary_inputs,
                )
                identity = payload["supplyChain"]["signing"]["signerIdentity"]
                if mutation is None:
                    del payload["supplyChain"]["signing"]["signerIdentity"]
                else:
                    field, value = mutation
                    identity[field] = value
                report_path = root / "release.json"
                report_path.write_text(json.dumps(payload), encoding="utf-8")
                with self.assertRaises(gate.ReleaseGateError) as caught:
                    gate.load_registry_release_evidence(report_path)

                self.assertEqual(
                    caught.exception.code,
                    "release.vault_kms_registry_release_gate_invalid",
                )

    def test_rejects_missing_production_signing_source_hashes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            production_registry_boundary, production_registry_boundary_inputs = (
                production_registry_boundary_bundle(root)
            )
            payload = registry_release_gate_report_payload(
                production_registry_boundary=production_registry_boundary,
                production_registry_boundary_inputs=production_registry_boundary_inputs,
            )
            del payload["source"]["supplyChain"]["productionSigningProfile"]["sha256"]
            report_path = root / "release.json"
            report_path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.load_registry_release_evidence(report_path)

        self.assertEqual(
            caught.exception.code,
            "release.vault_kms_registry_release_gate_invalid",
        )

    def test_rejects_missing_release_source_hash_set(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            production_registry_boundary, production_registry_boundary_inputs = (
                production_registry_boundary_bundle(root)
            )
            payload = registry_release_gate_report_payload(
                production_registry_boundary=production_registry_boundary,
                production_registry_boundary_inputs=production_registry_boundary_inputs,
            )
            del payload["source"]["sourceHashes"]
            report_path = root / "release.json"
            report_path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.load_registry_release_evidence(report_path)

        self.assertEqual(caught.exception.code, "release.vault_kms_registry_release_gate_invalid")

    def test_rejects_cached_signature_annotation_mismatch(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            production_registry_boundary, production_registry_boundary_inputs = (
                production_registry_boundary_bundle(root)
            )
            payload = registry_release_gate_report_payload(
                signatures=[
                    cached_signature_report(version="0.5.3"),
                ],
                production_registry_boundary=production_registry_boundary,
                production_registry_boundary_inputs=production_registry_boundary_inputs,
            )
            report_path = root / "release.json"
            report_path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.load_registry_release_evidence(report_path)

        self.assertEqual(caught.exception.code, "release.vault_kms_registry_release_gate_invalid")

    def test_rejects_missing_cached_transparency_log_conclusion(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            production_registry_boundary, production_registry_boundary_inputs = (
                production_registry_boundary_bundle(root)
            )
            payload = registry_release_gate_report_payload(
                production_registry_boundary=production_registry_boundary,
                production_registry_boundary_inputs=production_registry_boundary_inputs,
            )
            payload["supplyChain"]["signing"]["transparencyLogInclusionProofPresent"] = False
            report_path = root / "release.json"
            report_path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.load_registry_release_evidence(report_path)

        self.assertEqual(caught.exception.code, "release.vault_kms_registry_release_gate_invalid")

    def test_rejects_stale_live_runtime_evidence(self) -> None:
        stale_collected_at = (
            dt.datetime.now(dt.timezone.utc)
            - dt.timedelta(
                seconds=gate.registry_gate.PRODUCTION_REGISTRY_RUNTIME_EVIDENCE_MAX_AGE_SECONDS + 1
            )
        ).isoformat()
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            production_registry_boundary, production_registry_boundary_inputs = (
                production_registry_boundary_bundle(root, collected_at=stale_collected_at)
            )
            payload = registry_release_gate_report_payload(
                production_registry_boundary=production_registry_boundary,
                production_registry_boundary_inputs=production_registry_boundary_inputs,
            )
            report_path = root / "release.json"
            report_path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.load_registry_release_evidence(report_path)

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_rejects_registry_config_drift_against_exported_runtime_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            production_registry_boundary, production_registry_boundary_inputs = (
                production_registry_boundary_bundle(root)
            )
            registry_config_path = pathlib.Path(
                production_registry_boundary["registryConfigPath"]
            )
            registry_config_path.write_text(
                registry_config_path.read_text(encoding="utf-8") + "# drift\n",
                encoding="utf-8",
            )
            payload = registry_release_gate_report_payload(
                production_registry_boundary=production_registry_boundary,
                production_registry_boundary_inputs=production_registry_boundary_inputs,
            )
            report_path = root / "release.json"
            report_path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.load_registry_release_evidence(report_path)

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_rejects_retention_policy_runtime_config_path_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            production_registry_boundary, production_registry_boundary_inputs = (
                production_registry_boundary_bundle(root)
            )
            retention_policy_path = pathlib.Path(
                production_registry_boundary["retentionPolicyPath"]
            )
            payload = json.loads(retention_policy_path.read_text(encoding="utf-8"))
            payload["registryConfigPath"] = "/etc/distribution/other-config.yml"
            retention_policy_path.write_text(json.dumps(payload), encoding="utf-8")
            report_payload = registry_release_gate_report_payload(
                production_registry_boundary=production_registry_boundary,
                production_registry_boundary_inputs=production_registry_boundary_inputs,
            )
            report_path = root / "release.json"
            report_path.write_text(json.dumps(report_payload), encoding="utf-8")

            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.load_registry_release_evidence(report_path)

        self.assertEqual(caught.exception.code, "release.vault_kms_registry_release_gate_invalid")


class ReleaseEvidenceBoundaryTest(unittest.TestCase):
    def test_rejects_source_hash_drift_against_current_source(self) -> None:
        configuration = production_configuration()
        drifted_hashes = current_release_source_hashes()
        first_path = next(iter(drifted_hashes))
        drifted_hashes[first_path] = "0" * 64

        with self.assertRaises(gate.ReleaseGateError) as caught:
            gate.validate_release_evidence_against_source(
                release_evidence(source_hashes=drifted_hashes),
                source={"gitSha": GIT_SHA},
                configuration=configuration,
            )

        self.assertEqual(caught.exception.code, "release.vault_kms_registry_release_gate_invalid")
        self.assertEqual(caught.exception.evidence["sourceHashMismatchCount"], 1)
        self.assertIn(first_path, caught.exception.evidence["sourceHashMismatches"])

    def test_rejects_explicit_signed_image_digest_mismatch(self) -> None:
        with self.assertRaises(gate.ReleaseGateError) as caught:
            gate.choose_signed_image(
                f"{IMAGE_REPOSITORY}@{'sha256:' + '9' * 64}",
                release_evidence(),
            )

        self.assertEqual(caught.exception.code, "release.vault_kms_image_identity_invalid")

    def test_rejects_tls_runtime_evidence_mismatch_against_live_probe(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            production_registry_boundary, _production_registry_boundary_inputs = (
                production_registry_boundary_bundle(pathlib.Path(directory))
            )
        with self.assertRaises(gate.ReleaseGateError) as caught:
            gate.validate_registry_runtime_evidence_against_live_boundary(
                release_evidence(production_registry_boundary=production_registry_boundary),
                options=gate_options(pathlib.Path(tempfile.gettempdir()) / "vault-runtime-cross-check"),
                registry_boundary={
                    "registryHost": "registry.example.test",
                    "tlsCertificateSha256": "f" * 64,
                    "principalEnvironment": "REGISTRY_USERNAME",
                    "passwordEnvironment": "REGISTRY_PASSWORD",
                    "caCertificateEnvironment": "REGISTRY_CA_CERT",
                },
            )

        self.assertEqual(caught.exception.code, "release.vault_kms_registry_boundary_invalid")

    def test_rejects_registry_access_env_drift_against_live_probe(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            production_registry_boundary, _production_registry_boundary_inputs = (
                production_registry_boundary_bundle(pathlib.Path(directory))
            )
        with self.assertRaises(gate.ReleaseGateError) as caught:
            gate.validate_registry_runtime_evidence_against_live_boundary(
                release_evidence(production_registry_boundary=production_registry_boundary),
                options=gate_options(pathlib.Path(tempfile.gettempdir()) / "vault-runtime-cross-check"),
                registry_boundary={
                    "registryHost": "registry.example.test",
                    "tlsCertificateSha256": "e" * 64,
                    "principalEnvironment": "OTHER_REGISTRY_USERNAME",
                    "passwordEnvironment": "REGISTRY_PASSWORD",
                    "caCertificateEnvironment": "REGISTRY_CA_CERT",
                },
            )

        self.assertEqual(caught.exception.code, "release.vault_kms_registry_boundary_invalid")


class ClusterPolicyValidationTest(unittest.TestCase):
    def test_accepts_static_repository_pattern_and_public_key_namespace(self) -> None:
        bundle, _public_key_cm, _repository_cm, cluster_policy = gate.render_admission_bundle(
            run_id="run-1",
            namespace="synara-system",
            repository_pattern=f"{IMAGE_REPOSITORY}*",
            public_key=PUBLIC_KEY,
            source_hashes={"deploy/worker/production-signing-policy.json": "c" * 64},
        )
        spec = copy.deepcopy(cluster_policy["spec"])
        contexts = spec["rules"][0]["context"]
        contexts[0]["configMap"]["namespace"] = "kyverno-keys"

        gate.validate_cluster_policy_spec(
            spec,
            public_key_configmap_namespace="kyverno-keys",
            public_key_configmap_name=bundle.names.public_key_configmap,
            repository_pattern=f"{IMAGE_REPOSITORY}*",
        )

    def test_rejects_attestor_boundary_drift(self) -> None:
        _bundle, _public_key_cm, _repository_cm, cluster_policy = gate.render_admission_bundle(
            run_id="run-2",
            namespace="synara-system",
            repository_pattern=f"{IMAGE_REPOSITORY}*",
            public_key=PUBLIC_KEY,
            source_hashes={"deploy/worker/production-signing-policy.json": "c" * 64},
        )
        spec = copy.deepcopy(cluster_policy["spec"])
        spec["rules"][0]["verifyImages"][0]["attestors"][0]["entries"][0]["keys"][
            "publicKeys"
        ] = "inline-key-material"

        with self.assertRaises(gate.ReleaseGateError) as caught:
            gate.validate_cluster_policy_spec(
                spec,
                public_key_configmap_namespace="synara-system",
                public_key_configmap_name="synara-worker-cosign-public-key",
                repository_pattern=f"{IMAGE_REPOSITORY}*",
            )

        self.assertEqual(caught.exception.code, "release.vault_kms_admission_invalid")

    def test_rejects_attestor_transparency_log_drift(self) -> None:
        _bundle, _public_key_cm, _repository_cm, cluster_policy = gate.render_admission_bundle(
            run_id="run-tlog-drift",
            namespace="synara-system",
            repository_pattern=f"{IMAGE_REPOSITORY}*",
            public_key=PUBLIC_KEY,
            source_hashes={"deploy/worker/production-signing-policy.json": "c" * 64},
        )
        spec = copy.deepcopy(cluster_policy["spec"])
        keys = spec["rules"][0]["verifyImages"][0]["attestors"][0]["entries"][0]["keys"]
        keys["rekor"]["ignoreTlog"] = True

        with self.assertRaises(gate.ReleaseGateError) as caught:
            gate.validate_cluster_policy_spec(
                spec,
                public_key_configmap_namespace="synara-system",
                public_key_configmap_name="synara-worker-cosign-public-key",
                repository_pattern=f"{IMAGE_REPOSITORY}*",
            )

        self.assertEqual(caught.exception.code, "release.vault_kms_admission_invalid")

    def test_verify_existing_resources_uses_profile_specific_namespaces(self) -> None:
        configuration = production_configuration()
        profile = configuration.production_signing_profile
        assert profile is not None
        shifted_profile = dataclasses.replace(
            profile,
            public_key_configmap_namespace="kyverno-keys",
            repository_configmap_namespace="kyverno-repositories",
        )
        spec = copy.deepcopy(
            gate.render_admission_bundle(
                run_id="run-3",
                namespace="synara-system",
                repository_pattern=f"{IMAGE_REPOSITORY}*",
                public_key=PUBLIC_KEY,
                source_hashes={"deploy/worker/production-signing-policy.json": "c" * 64},
            )[3]["spec"]
        )
        spec["rules"][0]["context"][0]["configMap"]["name"] = shifted_profile.public_key_configmap_name
        spec["rules"][0]["context"][0]["configMap"]["namespace"] = "kyverno-keys"
        options = gate_options(pathlib.Path(tempfile.gettempdir()) / "synara-vault-kms-existing")

        with mock.patch.object(
            gate,
            "kubectl_json",
            side_effect=[
                {"data": {shifted_profile.public_key_configmap_key: PUBLIC_KEY}},
                {"data": {shifted_profile.repository_configmap_key: f"{IMAGE_REPOSITORY}*"}},
                {"spec": spec},
            ],
        ):
            bundle = gate.verify_existing_admission_resources(
                options,
                profile=shifted_profile,
                public_key=PUBLIC_KEY,
                repository_pattern=f"{IMAGE_REPOSITORY}*",
                source_hashes={"deploy/worker/production-signing-policy.json": "c" * 64},
                redactor=acceptance.SecretRedactor(),
            )

        self.assertEqual(bundle.mode, "verify-existing")
        self.assertEqual(bundle.names.public_key_configmap, shifted_profile.public_key_configmap_name)

    def test_verify_live_cluster_policy_requires_autogen_controller_annotation(self) -> None:
        bundle, _public_key_cm, _repository_cm, cluster_policy = gate.render_admission_bundle(
            run_id="run-4",
            namespace="synara-system",
            repository_pattern=f"{IMAGE_REPOSITORY}*",
            public_key=PUBLIC_KEY,
            source_hashes={"deploy/worker/production-signing-policy.json": "c" * 64},
        )
        cluster_policy["metadata"]["annotations"].pop(gate.AUTOGEN_CONTROLLERS_ANNOTATION, None)
        options = gate_options(pathlib.Path(tempfile.gettempdir()) / "synara-vault-kms-live-policy")

        with (
            mock.patch.object(gate, "kubectl_json", return_value=cluster_policy),
            self.assertRaises(gate.ReleaseGateError) as caught,
        ):
            gate.verify_live_cluster_policy(
                options,
                cluster_policy_name=str(bundle.names.cluster_policy),
                public_key_configmap_namespace="synara-system",
                public_key_configmap_name=str(bundle.names.public_key_configmap),
                repository_pattern=f"{IMAGE_REPOSITORY}*",
                redactor=acceptance.SecretRedactor(),
            )

        self.assertEqual(caught.exception.code, "release.vault_kms_admission_invalid")


class SecretHandlingTest(unittest.TestCase):
    def test_prepare_secret_inputs_materializes_private_files_and_redacts_values(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            state_dir = root / "state"
            vault_ca = root / "vault-ca.crt"
            registry_ca = root / "registry-ca.crt"
            ca_bytes = b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"
            vault_ca.write_bytes(ca_bytes)
            registry_ca.write_bytes(ca_bytes)
            options = gate_options(root / "output")
            redactor = acceptance.SecretRedactor()
            basic_auth_b64 = base64.b64encode(b"registry-user:registry-password").decode("ascii")

            with mock.patch.dict(
                gate.os.environ,
                {
                    "VAULT_ADDR": "https://vault.example.test",
                    "VAULT_TOKEN": "vault-signer-token",
                    "VAULT_OPERATOR_TOKEN": "vault-operator-token",
                    "VAULT_CACERT": str(vault_ca),
                    "REGISTRY_CA_CERT": str(registry_ca),
                    "REGISTRY_USERNAME": "registry-user",
                    "REGISTRY_PASSWORD": "registry-password",
                },
            ):
                secret_inputs = gate.prepare_secret_inputs(
                    options,
                    state_dir=state_dir,
                    redactor=redactor,
                )

                self.assertEqual(
                    secret_inputs.vault_environment["VAULT_ADDR"],
                    "https://vault.example.test",
                )
                self.assertNotEqual(secret_inputs.vault_environment["VAULT_CACERT"], str(vault_ca))
                self.assertTrue(secret_inputs.registry_ca_path.is_file())
                self.assertEqual(
                    stat.S_IMODE(secret_inputs.registry_ca_path.stat().st_mode),
                    0o600,
                )
                serialized = json.dumps(
                    redactor.value(
                        {
                            "signerToken": "vault-signer-token",
                            "operatorToken": "vault-operator-token",
                            "username": "registry-user",
                            "password": "registry-password",
                            "basicAuth": "registry-user:registry-password",
                            "basicAuthB64": basic_auth_b64,
                        }
                    ),
                    sort_keys=True,
                )
                self.assertNotIn("vault-signer-token", serialized)
                self.assertNotIn("vault-operator-token", serialized)
                self.assertNotIn("registry-user", serialized)
                self.assertNotIn("registry-password", serialized)
                self.assertNotIn("registry-user:registry-password", serialized)
                self.assertNotIn(basic_auth_b64, serialized)

    def test_prepare_secret_inputs_rejects_equal_signer_and_operator_tokens(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            vault_ca = root / "vault-ca.crt"
            registry_ca = root / "registry-ca.crt"
            vault_ca.write_text("ca", encoding="utf-8")
            registry_ca.write_text("ca", encoding="utf-8")
            options = gate_options(root / "output")
            with (
                mock.patch.dict(
                    gate.os.environ,
                    {
                        "VAULT_ADDR": "https://vault.example.test",
                        "VAULT_TOKEN": "same-token",
                        "VAULT_OPERATOR_TOKEN": "same-token",
                        "VAULT_CACERT": str(vault_ca),
                        "REGISTRY_CA_CERT": str(registry_ca),
                        "REGISTRY_USERNAME": "registry-user",
                        "REGISTRY_PASSWORD": "registry-password",
                    },
                ),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.prepare_secret_inputs(
                    options,
                    state_dir=root / "state",
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_identity_invalid")


class ProbeAccessTest(unittest.TestCase):
    def test_render_probe_access_resources_scopes_pull_secret_to_kyverno_service_accounts(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            secret_inputs = vault_secret_inputs(pathlib.Path(directory))
            redactor = acceptance.SecretRedactor()
            names, rendered_hashes, created_resources, documents = gate.render_probe_access_resources(
                run_id="run-1",
                namespace="synara-admission",
                registry_host="registry.example.test",
                secret_inputs=secret_inputs,
                kyverno_runtime=kyverno_runtime(),
                redactor=redactor,
            )

        self.assertIsNotNone(names.probe_pull_secret)
        self.assertEqual(created_resources[0][0], "secret")
        self.assertIn("probePullSecret", rendered_hashes)
        self.assertEqual(documents[0]["type"], "kubernetes.io/dockerconfigjson")
        self.assertEqual(documents[1]["rules"][0]["verbs"], ["get"])
        self.assertEqual(
            [subject["name"] for subject in documents[2]["subjects"]],
            ["kyverno-admission-controller", "kyverno-background-controller"],
        )

    def test_dry_run_pod_manifest_sets_image_pull_secret(self) -> None:
        manifest = json.loads(
            gate._dry_run_pod_manifest(
                name="probe",
                namespace="synara-admission",
                image=f"{IMAGE_REPOSITORY}@{DIGEST}",
                run_id="run-1",
                image_pull_secret_name="registry-pull-secret",
            )
        )

        self.assertEqual(
            manifest["spec"]["imagePullSecrets"],
            [{"name": "registry-pull-secret"}],
        )

    def test_dry_run_workload_manifest_sets_controller_pull_secret(self) -> None:
        for kind in gate.REQUIRED_CONTROLLER_PROBE_KINDS:
            manifest = json.loads(
                gate._dry_run_workload_manifest(
                    name=f"probe-{kind.lower()}",
                    namespace="synara-admission",
                    image=f"{IMAGE_REPOSITORY}@{DIGEST}",
                    run_id="run-1",
                    image_pull_secret_name="registry-pull-secret",
                    resource_kind=kind,
                )
            )
            if kind == "CronJob":
                template_spec = manifest["spec"]["jobTemplate"]["spec"]["template"]["spec"]
            elif kind == "Job":
                template_spec = manifest["spec"]["template"]["spec"]
            else:
                template_spec = manifest["spec"]["template"]["spec"]
            self.assertEqual(
                template_spec["imagePullSecrets"],
                [{"name": "registry-pull-secret"}],
            )


class KyvernoControllerMutationTest(unittest.TestCase):
    def test_verify_kyverno_runtime_reports_verify_image_controllers(self) -> None:
        options = gate_options(pathlib.Path(tempfile.gettempdir()) / "synara-kyverno-runtime")
        with mock.patch.object(
            gate,
            "kubectl_json",
            return_value={
                "items": [
                    {
                        "metadata": {
                            "namespace": "kyverno",
                            "name": "kyverno-admission-controller",
                            "labels": {"app.kubernetes.io/component": "admission-controller"},
                        },
                        "spec": {"template": {"spec": {"serviceAccountName": "kyverno-admission-controller"}}},
                        "status": {"availableReplicas": 1},
                    },
                    {
                        "metadata": {
                            "namespace": "kyverno",
                            "name": "kyverno-background-controller",
                            "labels": {"app.kubernetes.io/component": "background-controller"},
                        },
                        "spec": {"template": {"spec": {"serviceAccountName": "kyverno-background-controller"}}},
                        "status": {"availableReplicas": 1},
                    },
                ]
            },
        ):
            evidence = gate.verify_kyverno_runtime(options, redactor=acceptance.SecretRedactor())

        self.assertEqual(len(evidence["verifyImageControllers"]), 2)
        self.assertEqual(
            evidence["verifyImageControllers"][0]["component"],
            "admission-controller",
        )

    def test_apply_owned_bundle_injects_controller_ca_and_cleanup_restores_it(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            ca_path = root / "registry-ca.crt"
            ca_path.write_text(
                "-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n",
                encoding="utf-8",
            )
            secret_inputs = dataclasses.replace(vault_secret_inputs(root), registry_ca_path=ca_path)
            bundle, public_key_cm, repository_cm, cluster_policy = gate.render_admission_bundle(
                run_id="run-1",
                namespace="synara-system",
                repository_pattern=f"{IMAGE_REPOSITORY}*",
                public_key=PUBLIC_KEY,
                source_hashes={"deploy/worker/production-signing-policy.json": "c" * 64},
            )
            capture: list[dict[str, Any]] = []

            def capture_apply(
                _options: gate.GateOptions,
                bundle_objects: list[dict[str, Any]],
                *,
                redactor: acceptance.SecretRedactor,
            ) -> None:
                del redactor
                capture.extend(copy.deepcopy(bundle_objects))

            deployment = {
                "apiVersion": "apps/v1",
                "kind": "Deployment",
                "metadata": {"name": "kyverno-admission-controller", "namespace": "kyverno", "annotations": {}},
                "spec": {
                    "template": {
                        "metadata": {"annotations": {}},
                        "spec": {
                            "containers": [{"name": "kyverno", "volumeMounts": []}],
                            "volumes": [],
                        },
                    }
                },
            }
            options = gate_options(root / "output", admission_mode="apply-owned")
            runtime = {
                **kyverno_runtime(),
                "verifyImageControllers": [kyverno_runtime()["verifyImageControllers"][0]],
            }

            with (
                mock.patch.object(gate, "_apply_resource_documents", side_effect=capture_apply),
                mock.patch.object(gate, "kubectl_json", return_value=copy.deepcopy(deployment)),
                mock.patch.object(
                    gate,
                    "kubectl_completed",
                    return_value=subprocess.CompletedProcess(["kubectl"], 0, stdout="", stderr=""),
                ),
            ):
                applied_bundle = gate.apply_owned_admission_bundle(
                    options,
                    bundle,
                    [public_key_cm, repository_cm, cluster_policy],
                    run_id="run-1",
                    kyverno_runtime=runtime,
                    secret_inputs=secret_inputs,
                    redactor=acceptance.SecretRedactor(),
                )

            self.assertEqual(len(applied_bundle.controller_patches), 1)
            self.assertTrue(applied_bundle.controller_patches[0].injected_mount)
            self.assertTrue(
                any(
                    document.get("kind") == "ConfigMap"
                    and document.get("metadata", {}).get("name", "").startswith("synara-kyverno-registry-ca-")
                    for document in capture
                )
            )
            deployment_apply = next(document for document in capture if document.get("kind") == "Deployment")
            mounts = deployment_apply["spec"]["template"]["spec"]["containers"][0]["volumeMounts"]
            self.assertEqual(mounts[0]["mountPath"], gate.KYVERNO_CA_MOUNT_PATH)

            cleanup_capture: list[dict[str, Any]] = []

            def capture_cleanup_apply(
                _options: gate.GateOptions,
                bundle_objects: list[dict[str, Any]],
                *,
                redactor: acceptance.SecretRedactor,
            ) -> None:
                del redactor
                cleanup_capture.extend(copy.deepcopy(bundle_objects))

            deployment_for_cleanup = copy.deepcopy(deployment_apply)
            with (
                mock.patch.object(gate, "_apply_resource_documents", side_effect=capture_cleanup_apply),
                mock.patch.object(gate, "kubectl_json", return_value=deployment_for_cleanup),
                mock.patch.object(gate, "_resource_owner", return_value="run-1"),
                mock.patch.object(
                    gate,
                    "kubectl_completed",
                    return_value=subprocess.CompletedProcess(["kubectl"], 0, stdout="", stderr=""),
                ),
            ):
                cleanup = gate.cleanup_owned_resources(
                    options,
                    applied_bundle,
                    run_id="run-1",
                    redactor=acceptance.SecretRedactor(),
                )

            restored_deployment = next(document for document in cleanup_capture if document.get("kind") == "Deployment")
            self.assertEqual(restored_deployment["spec"]["template"]["spec"]["volumes"], [])
            self.assertEqual(
                restored_deployment["spec"]["template"]["spec"]["containers"][0]["volumeMounts"],
                [],
            )
            self.assertTrue(cleanup["exactOwnerCleanup"])


class VaultPolicyBoundaryTest(unittest.TestCase):
    @staticmethod
    def _write_vault_baseline(
        root: pathlib.Path,
        *,
        values_text: str | None = None,
        bootstrap_text: str | None = None,
        audit_observability_text: str | None = None,
        operations_policy_payload: dict[str, Any] | None = None,
        helm_post_renderer_text: str | None = None,
        helm_post_renderer_plugin_text: str | None = None,
    ) -> None:
        for relative_path in (
            gate.VAULT_VALUES_PRODUCTION_PATH,
            gate.VAULT_SERVER_PDB_PATH,
            gate.VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_PATH,
            gate.VAULT_OPERATIONS_POLICY_PATH,
            gate.VAULT_BOOTSTRAP_PATH,
            gate.VAULT_HELM_POST_RENDERER_PATH,
            gate.VAULT_HELM_POST_RENDERER_PLUGIN_PATH,
        ):
            destination = root / relative_path
            destination.parent.mkdir(parents=True, exist_ok=True)
            if (
                relative_path == gate.VAULT_OPERATIONS_POLICY_PATH
                and operations_policy_payload is not None
            ):
                destination.write_text(
                    json.dumps(operations_policy_payload, indent=2) + "\n",
                    encoding="utf-8",
                )
                continue
            source_text = (REPO_ROOT / relative_path).read_text(encoding="utf-8")
            if relative_path == gate.VAULT_VALUES_PRODUCTION_PATH and values_text is not None:
                source_text = values_text
            if (
                relative_path == gate.VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_PATH
                and audit_observability_text is not None
            ):
                source_text = audit_observability_text
            if relative_path == gate.VAULT_BOOTSTRAP_PATH and bootstrap_text is not None:
                source_text = bootstrap_text
            if (
                relative_path == gate.VAULT_HELM_POST_RENDERER_PATH
                and helm_post_renderer_text is not None
            ):
                source_text = helm_post_renderer_text
            if (
                relative_path == gate.VAULT_HELM_POST_RENDERER_PLUGIN_PATH
                and helm_post_renderer_plugin_text is not None
            ):
                source_text = helm_post_renderer_plugin_text
            destination.write_text(source_text, encoding="utf-8")

    def test_accepts_live_policy_text_after_normalization(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            secret_inputs = vault_secret_inputs(pathlib.Path(directory))
            live_policy = "# comment\n\n" + "\n".join(
                f"  {line.strip()}  "
                for line in (
                    REPO_ROOT / gate.VAULT_SIGNER_POLICY_PATH
                ).read_text(encoding="utf-8").splitlines()
                if line.strip()
            )

            with (
                mock.patch.object(
                    gate,
                    "kubectl_json",
                    side_effect=vault_kubectl_side_effect(),
                ),
                mock.patch.object(
                    gate,
                    "vault_json",
                    side_effect=vault_json_side_effect(),
                ) as vault_json_mock,
                mock.patch.object(
                    gate,
                    "vault_text",
                    side_effect=[
                        live_policy,
                        (REPO_ROOT / gate.VAULT_OPERATOR_POLICY_PATH).read_text(encoding="utf-8"),
                    ],
                ) as vault_text_mock,
            ):
                evidence = gate.verify_vault_cluster(
                    options,
                    secret_inputs,
                    redactor=acceptance.SecretRedactor(),
                )

            self.assertTrue(
                all(
                    call.kwargs["vault_environment"]
                    is secret_inputs.vault_operator_environment
                    for call in vault_json_mock.call_args_list[:7]
                )
            )
            self.assertTrue(
                all(
                    call.kwargs["vault_environment"] is secret_inputs.vault_environment
                    for call in vault_json_mock.call_args_list[7:]
                )
            )
            self.assertTrue(
                all(
                    call.kwargs["vault_environment"]
                    is secret_inputs.vault_operator_environment
                    for call in vault_text_mock.call_args_list
                )
            )

        self.assertEqual(evidence["vault"]["policyName"], gate.VAULT_SIGNER_POLICY_NAME)
        self.assertEqual(
            evidence["vault"]["identities"]["signer"]["tokenEnvironment"],
            "VAULT_TOKEN",
        )

    def test_rejects_live_policy_text_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            secret_inputs = vault_secret_inputs(pathlib.Path(directory))
            drifted_policy = (
                (REPO_ROOT / gate.VAULT_SIGNER_POLICY_PATH).read_text(encoding="utf-8")
                + '\npath "sys/mounts" {\n  capabilities = ["read"]\n}\n'
            )

            with (
                mock.patch.object(
                    gate,
                    "kubectl_json",
                    side_effect=vault_kubectl_side_effect(),
                ),
                mock.patch.object(
                    gate,
                    "vault_json",
                    side_effect=vault_json_side_effect(),
                ),
                mock.patch.object(
                    gate,
                    "vault_text",
                    side_effect=[
                        drifted_policy,
                        (REPO_ROOT / gate.VAULT_OPERATOR_POLICY_PATH).read_text(encoding="utf-8"),
                    ],
                ),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    secret_inputs,
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_policy_invalid")

    def test_rejects_exportable_live_transit_signing_key(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            secret_inputs = vault_secret_inputs(pathlib.Path(directory))
            vault_payloads = vault_json_side_effect()
            vault_payloads[3]["data"]["exportable"] = True
            with (
                mock.patch.object(
                    gate,
                    "kubectl_json",
                    side_effect=vault_kubectl_side_effect(),
                ),
                mock.patch.object(gate, "vault_json", side_effect=vault_payloads),
                mock.patch.object(
                    gate,
                    "vault_text",
                    side_effect=[
                        (REPO_ROOT / gate.VAULT_SIGNER_POLICY_PATH).read_text(encoding="utf-8"),
                        (REPO_ROOT / gate.VAULT_OPERATOR_POLICY_PATH).read_text(encoding="utf-8"),
                    ],
                ),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    secret_inputs,
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_key_invalid")

    def test_rejects_statefulset_image_drift_from_values_pin(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            secret_inputs = vault_secret_inputs(pathlib.Path(directory))
            kubectl_payloads = vault_kubectl_side_effect()
            kubectl_payloads[2]["items"][0]["spec"]["template"]["spec"]["containers"][0]["image"] = (
                "hashicorp/vault:2.0.2@sha256:" + "9" * 64
            )

            with (
                mock.patch.object(gate, "kubectl_json", side_effect=kubectl_payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    secret_inputs,
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_kubernetes_invalid")

    def test_rejects_source_peer_network_or_bootstrap_identity_drift(self) -> None:
        values = (REPO_ROOT / gate.VAULT_VALUES_PRODUCTION_PATH).read_text(encoding="utf-8")
        bootstrap = (REPO_ROOT / gate.VAULT_BOOTSTRAP_PATH).read_text(encoding="utf-8")
        helm_post_renderer = (REPO_ROOT / gate.VAULT_HELM_POST_RENDERER_PATH).read_text(
            encoding="utf-8"
        )
        helm_post_renderer_plugin = (
            REPO_ROOT / gate.VAULT_HELM_POST_RENDERER_PLUGIN_PATH
        ).read_text(encoding="utf-8")
        cases = {
            "peer-network": {
                "values_text": values.replace(
                    "app.kubernetes.io/instance: synara-vault",
                    "app.kubernetes.io/instance: drifted-vault",
                    1,
                )
            },
            "vault-cacert": {
                "values_text": values.replace(
                    "VAULT_CACERT: /vault/tls/ca.crt",
                    "VAULT_CACERT: /tmp/unknown-ca.crt",
                    1,
                )
            },
            "liveness-tls-bypass": {
                "values_text": values.replace(
                    "vault status >/dev/null",
                    "vault status -tls-skip-verify >/dev/null",
                    1,
                )
            },
            "liveness-catchup-window": {
                "values_text": values.replace(
                    "initialDelaySeconds: 300",
                    "initialDelaySeconds: 60",
                    1,
                )
            },
            "readiness-health-path": {
                "values_text": values.replace(
                    "path: /v1/sys/health?standbyok=true&perfstandbyok=true&sealedcode=204&uninitcode=204",
                    "path: /v1/sys/health?standbyok=true",
                    1,
                )
            },
            "readiness-post-renderer-command": {
                "helm_post_renderer_text": helm_post_renderer.replace(
                    'print indentation "      - \\\"vault status >/dev/null\\\""',
                    'print indentation "      - \\\"vault status -tls-skip-verify >/dev/null\\\""',
                    1,
                )
            },
            "readiness-post-renderer-plugin-type": {
                "helm_post_renderer_plugin_text": helm_post_renderer_plugin.replace(
                    "type: postrenderer/v1",
                    "type: cli/v1",
                    1,
                )
            },
            "share-process-namespace": {
                "values_text": values.replace(
                    "shareProcessNamespace: true",
                    "shareProcessNamespace: false",
                    1,
                )
            },
            "retry-join-duplicate": {
                "values_text": values.replace(
                    "https://synara-vault-2.synara-vault-internal:8200",
                    "https://synara-vault-1.synara-vault-internal:8200",
                    1,
                )
            },
            "dns-egress-selector": {
                "values_text": values.replace(
                    "k8s-app: kube-dns",
                    "k8s-app: drifted-dns",
                    1,
                )
            },
            "api-egress-6443": {
                "values_text": values.replace(
                    "          - port: 6443\n            protocol: TCP\n",
                    "",
                    1,
                )
            },
            "audit-secondary-default": {
                "bootstrap_text": bootstrap.replace(
                    'AUDIT_DEVICE_PATH_SECONDARY="${AUDIT_DEVICE_PATH_SECONDARY:-file-secondary}"',
                    'AUDIT_DEVICE_PATH_SECONDARY="${AUDIT_DEVICE_PATH_SECONDARY:-file-json}"',
                    1,
                )
            },
            "bootstrap-token-type": {
                "bootstrap_text": bootstrap.replace("token_type=batch", "token_type=service", 1)
            },
            "bootstrap-snapshot-approle": {
                "bootstrap_text": bootstrap.replace(
                    'SNAPSHOT_APPROLE_NAME="${VAULT_SNAPSHOT_APPROLE_NAME:-synara-vault-snapshot-operator}"',
                    'SNAPSHOT_APPROLE_NAME="${VAULT_SNAPSHOT_APPROLE_NAME:-drifted-snapshot-operator}"',
                    1,
                )
            },
        }
        for case, overrides in cases.items():
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                self._write_vault_baseline(root, **overrides)
                with self.assertRaises(gate.ReleaseGateError) as caught:
                    gate._load_vault_baseline(root)

                self.assertEqual(caught.exception.code, "release.vault_kms_source_invalid")

    def test_rejects_source_audit_observability_or_operations_policy_drift(self) -> None:
        audit_observability = (
            REPO_ROOT / gate.VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_PATH
        ).read_text(encoding="utf-8")
        operations_policy = json.loads(
            (REPO_ROOT / gate.VAULT_OPERATIONS_POLICY_PATH).read_text(encoding="utf-8")
        )
        cases = {
            "audit-shipper-secondary-input": {
                "audit_observability_text": audit_observability.replace(
                    "/vault/audit/audit-primary.log",
                    "/vault/audit/audit-secondary.log",
                    1,
                ),
            },
            "audit-shipper-missing-archive-include": {
                "audit_observability_text": audit_observability.replace(
                    "          - /vault/audit/audit-primary.log.*\n",
                    "",
                    1,
                ),
            },
            "audit-shipper-missing-gzip-exclude": {
                "audit_observability_text": audit_observability.replace(
                    "        exclude:\n          - /vault/audit/audit-primary.log.*.gz\n",
                    "",
                    1,
                ),
            },
            "audit-shipper-no-json-parse": {
                "audit_observability_text": audit_observability.replace(
                    "          .vault_audit = parse_json!(string!(.message))",
                    "          .parsed = string!(.message)",
                    1,
                ),
            },
            "audit-shipper-no-offset": {
                "audit_observability_text": audit_observability.replace(
                    "        offset_key: offset\n",
                    "",
                    1,
                ),
            },
            "rotation-copytruncate": {
                "audit_observability_text": audit_observability.replace(
                    '      mv -- "${current_file}" "${archived_file}"',
                    '      cp "${current_file}" "${archived_file}"',
                    1,
                ),
            },
            "rotation-missing-hup": {
                "audit_observability_text": audit_observability.replace(
                    '        kill -HUP "${vault_pid}"',
                    '        :',
                    1,
                ),
            },
            "rotation-missing-fd-wait": {
                "audit_observability_text": audit_observability.replace(
                    '        wait_for_vault_fd_rollover "${vault_pid}" "${rotation_plan}"',
                    '        :',
                    1,
                ),
            },
            "operations-policy-secret-name": {
                "operations_policy_payload": {
                    **operations_policy,
                    "audit": {
                        **operations_policy["audit"],
                        "externalSiem": {
                            **operations_policy["audit"]["externalSiem"],
                            "secretName": "drifted-vault-audit-siem",
                        },
                    },
                },
            },
            "operations-policy-share-process-namespace": {
                "operations_policy_payload": {
                    **operations_policy,
                    "audit": {
                        **operations_policy["audit"],
                        "localRotation": {
                            **operations_policy["audit"]["localRotation"],
                            "shareProcessNamespace": False,
                        },
                    },
                },
            },
            "operations-policy-exportable-transit-key": {
                "operations_policy_payload": {
                    **operations_policy,
                    "vault": {
                        **operations_policy["vault"],
                        "transitKey": {
                            **operations_policy["vault"]["transitKey"],
                            "exportable": True,
                        },
                    },
                },
            },
        }
        for case, overrides in cases.items():
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                self._write_vault_baseline(root, **overrides)
                with self.assertRaises(gate.ReleaseGateError) as caught:
                    gate._load_vault_baseline(root)

                self.assertEqual(caught.exception.code, "release.vault_kms_source_invalid")

    def test_checked_in_policies_keep_signing_and_auditing_separate(self) -> None:
        signer_policy = (REPO_ROOT / gate.VAULT_SIGNER_POLICY_PATH).read_text(encoding="utf-8")
        operator_policy = (REPO_ROOT / gate.VAULT_OPERATOR_POLICY_PATH).read_text(encoding="utf-8")

        self.assertIn('path "auth/token/lookup-self"', signer_policy)
        self.assertIn('path "transit/keys/synara-worker-release"', signer_policy)
        self.assertIn('path "transit/sign/synara-worker-release"', signer_policy)
        self.assertNotIn("transit/hmac", signer_policy)
        self.assertNotIn("transit/verify", signer_policy)
        self.assertIn('path "sys/storage/raft/configuration"', operator_policy)
        self.assertIn('path "sys/audit"', operator_policy)
        self.assertNotIn("transit/sign", operator_policy)

    def test_rejects_live_network_policy_peer_port_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            payloads[5]["spec"]["ingress"][0]["ports"] = [
                {"port": 8200, "protocol": "TCP"}
            ]
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_network_policy_invalid")

    def test_rejects_live_network_policy_without_control_plane_6443(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            api_egress = payloads[5]["spec"]["egress"][3]["ports"]
            payloads[5]["spec"]["egress"][3]["ports"] = [
                item for item in api_egress if item["port"] != 6443
            ]
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_network_policy_invalid")

    def test_rejects_live_network_policy_without_siem_egress(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            payloads[5]["spec"]["egress"] = [
                rule
                for rule in payloads[5]["spec"]["egress"]
                if rule["ports"][0]["port"] != gate.VAULT_AUDIT_SIEM_EGRESS_PORT
            ]
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_network_policy_invalid")

    def test_rejects_live_network_policy_with_unscoped_dns_egress(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            payloads[5]["spec"]["egress"][1]["to"] = []
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_network_policy_invalid")

    def test_rejects_live_vault_cacert_environment_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            container = payloads[2]["items"][0]["spec"]["template"]["spec"]["containers"][0]
            container["env"] = [item for item in container["env"] if item["name"] != "VAULT_CACERT"]
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_kubernetes_invalid")

    def test_rejects_live_liveness_command_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            container = payloads[2]["items"][0]["spec"]["template"]["spec"]["containers"][0]
            container["livenessProbe"]["exec"]["command"][-1] = (
                "vault status -tls-skip-verify >/dev/null 2>&1"
            )
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_kubernetes_invalid")

    def test_rejects_live_readiness_probe_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            container = payloads[2]["items"][0]["spec"]["template"]["spec"]["containers"][0]
            container["readinessProbe"] = {
                "httpGet": {
                    "path": gate.VAULT_READINESS_HEALTH_PATH,
                    "port": 8200,
                    "scheme": "HTTPS",
                },
                "failureThreshold": 3,
                "initialDelaySeconds": 300,
                "periodSeconds": 10,
                "successThreshold": 1,
                "timeoutSeconds": 3,
            }
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_kubernetes_invalid")

    def test_rejects_live_share_process_namespace_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            payloads[2]["items"][0]["spec"]["template"]["spec"]["shareProcessNamespace"] = False
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_kubernetes_invalid")

    def test_rejects_live_retry_join_tls_skip_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            configmap = payloads[-1]
            configmap["data"]["extraconfig-from-values.hcl"] = configmap["data"][
                "extraconfig-from-values.hcl"
            ].replace(
                'leader_ca_cert_file = "/vault/tls/ca.crt"',
                "leader_tls_skip_verify = \"true\"",
                1,
            )
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_kubernetes_invalid")

    def test_rejects_live_signer_identity_with_extra_default_policy(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            vault_payloads = vault_json_side_effect()
            vault_payloads[7]["data"]["policies"].append("default")
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=vault_kubectl_side_effect()),
                mock.patch.object(gate, "vault_json", side_effect=vault_payloads),
                mock.patch.object(
                    gate,
                    "vault_text",
                    side_effect=[
                        (REPO_ROOT / gate.VAULT_SIGNER_POLICY_PATH).read_text(encoding="utf-8"),
                        (REPO_ROOT / gate.VAULT_OPERATOR_POLICY_PATH).read_text(encoding="utf-8"),
                    ],
                ),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_identity_invalid")

    def test_rejects_live_vault_with_extra_audit_device(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            vault_payloads = vault_json_side_effect()
            vault_payloads[6]["file-json/"] = {
                "type": "file",
                "options": {"file_path": "/vault/audit/audit-json.log"},
            }
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=vault_kubectl_side_effect()),
                mock.patch.object(gate, "vault_json", side_effect=vault_payloads),
                mock.patch.object(
                    gate,
                    "vault_text",
                    side_effect=[
                        (REPO_ROOT / gate.VAULT_SIGNER_POLICY_PATH).read_text(encoding="utf-8"),
                        (REPO_ROOT / gate.VAULT_OPERATOR_POLICY_PATH).read_text(encoding="utf-8"),
                    ],
                ),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_audit_invalid")

    def test_rejects_live_vault_with_audit_path_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            vault_payloads = vault_json_side_effect()
            vault_payloads[6][f"{gate.VAULT_AUDIT_DEVICE_PRIMARY_NAME}/"]["options"]["file_path"] = (
                "/vault/audit/audit.log"
            )
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=vault_kubectl_side_effect()),
                mock.patch.object(gate, "vault_json", side_effect=vault_payloads),
                mock.patch.object(
                    gate,
                    "vault_text",
                    side_effect=[
                        (REPO_ROOT / gate.VAULT_SIGNER_POLICY_PATH).read_text(encoding="utf-8"),
                        (REPO_ROOT / gate.VAULT_OPERATOR_POLICY_PATH).read_text(encoding="utf-8"),
                    ],
                ),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_audit_invalid")

    def test_rejects_live_vault_audit_shipper_secret_mapping_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            shipper = payloads[2]["items"][0]["spec"]["template"]["spec"]["containers"][1]
            shipper["env"][0]["valueFrom"]["secretKeyRef"]["key"] = "DRIFTED_VAULT_AUDIT_SIEM_ENDPOINT"
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_kubernetes_invalid")

    def test_rejects_live_vault_audit_sidecar_uid_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            rotation = payloads[2]["items"][0]["spec"]["template"]["spec"]["containers"][2]
            rotation["securityContext"]["runAsUser"] = 1000
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_kubernetes_invalid")

    def test_rejects_live_vault_audit_observability_configmap_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            payloads = vault_kubectl_side_effect()
            payloads[6]["data"][gate.VAULT_AUDIT_VECTOR_CONFIG_KEY] = payloads[6]["data"][
                gate.VAULT_AUDIT_VECTOR_CONFIG_KEY
            ].replace(
                "/vault/audit/audit-primary.log",
                "/vault/audit/audit-primary-drift.log",
                1,
            )
            with (
                mock.patch.object(gate, "kubectl_json", side_effect=payloads),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_vault_cluster(
                    options,
                    vault_secret_inputs(pathlib.Path(directory)),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_kubernetes_invalid")

    def test_registry_signer_identity_must_match_live_lookup(self) -> None:
        evidence = release_evidence()
        matching = {
            "vault": {
                "identities": {
                    "signer": {
                        "roleName": evidence.signer_identity["roleName"],
                        "tokenType": evidence.signer_identity["type"],
                        "orphan": evidence.signer_identity["orphan"],
                        "policyHash": evidence.signer_identity["policiesSha256"],
                        "registryIdentitySha256": evidence.signer_identity["identitySha256"],
                    }
                }
            }
        }
        gate.validate_registry_signer_identity_against_live(evidence, matching)
        matching["vault"]["identities"]["signer"]["registryIdentitySha256"] = "0" * 64

        with self.assertRaises(gate.ReleaseGateError) as caught:
            gate.validate_registry_signer_identity_against_live(evidence, matching)

        self.assertEqual(caught.exception.code, "release.vault_kms_signer_identity_drift")


class PositiveSignatureBoundaryTest(unittest.TestCase):
    def test_rejects_signature_annotation_mismatch(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = gate_options(pathlib.Path(directory) / "output")
            public_key_path = pathlib.Path(directory) / "cosign.pub"
            public_key_path.write_text(PUBLIC_KEY, encoding="utf-8")
            secret_inputs = vault_secret_inputs(pathlib.Path(directory))
            payload = [
                {
                    "critical": {
                        "identity": {"docker-reference": f"{IMAGE_REPOSITORY}@{DIGEST}"},
                        "image": {"docker-manifest-digest": DIGEST},
                        "type": supply.COSIGN_CLAIM_TYPE,
                    },
                    "optional": {
                        "synara.git-sha": GIT_SHA,
                        "synara.run-id": "stage3-worker-registry-release-test",
                        "synara.slot": "cached",
                        "synara.version": "0.5.3",
                    },
                    "uuid": "123e4567-e89b-12d3-a456-426614174000",
                    "logIndex": 7,
                }
            ]

            with (
                mock.patch.object(
                    gate,
                    "cosign_completed",
                    return_value=subprocess.CompletedProcess(
                        ["cosign", "verify"],
                        0,
                        stdout=json.dumps(payload),
                        stderr="",
                    ),
                ),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.verify_positive_signature(
                    options,
                    secret_inputs,
                    public_key_path,
                    gate.normalize_image_reference(
                        f"{IMAGE_REPOSITORY}@{DIGEST}",
                        flag="signed image reference",
                        require_digest=True,
                    ),
                    release_evidence(),
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.vault_kms_cosign_invalid")


class RunVaultKmsAdmissionGateTest(unittest.TestCase):
    def test_emits_pass_report_for_verify_existing_mode(self) -> None:
        configuration = production_configuration()
        profile = configuration.production_signing_profile
        assert profile is not None

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            production_registry_boundary, _production_registry_boundary_inputs = (
                production_registry_boundary_bundle(root)
            )
            evidence = release_evidence(production_registry_boundary=production_registry_boundary)
            bundle = gate.AdmissionBundle(
                mode="verify-existing",
                namespace=profile.public_key_configmap_namespace,
                repository_pattern=f"{IMAGE_REPOSITORY}*",
                names=gate.AdmissionResourceNames(
                    public_key_configmap=profile.public_key_configmap_name,
                    repository_configmap=profile.repository_configmap_name,
                    cluster_policy=pathlib.Path(profile.cluster_policy_path).stem,
                ),
                source_hashes=dict(evidence.source_hashes),
                rendered_hashes={
                    "clusterPolicySpec": "a" * 64,
                    "publicKeyConfigMap": "b" * 64,
                    "repositoryConfigMap": "c" * 64,
                },
                created_resources=(),
            )
            output_dir = root / "gate-output"
            public_key_path = root / "cosign.pub"
            public_key_path.write_text(PUBLIC_KEY, encoding="utf-8")
            options = gate_options(output_dir)

            def fake_prepare(
                _options: gate.GateOptions,
                *,
                state_dir: pathlib.Path,
                redactor: acceptance.SecretRedactor,
            ) -> gate.SecretInputs:
                self.assertEqual(state_dir.parent, output_dir)
                redactor.add("vault-signer-token", "[REDACTED_VAULT_TOKEN]")
                redactor.add("vault-operator-token", "[REDACTED_VAULT_OPERATOR_TOKEN]")
                redactor.add("registry-password", "[REDACTED_REGISTRY_PASSWORD]")
                redactor.add(
                    "registry-user:registry-password",
                    "[REDACTED_REGISTRY_BASIC_AUTH]",
                )
                return vault_secret_inputs(state_dir)

            def fake_verify_existing(
                _options: gate.GateOptions,
                *,
                profile: supply.ProductionSigningProfile,
                public_key: str,
                repository_pattern: str,
                source_hashes: dict[str, str],
                redactor: acceptance.SecretRedactor,
            ) -> gate.AdmissionBundle:
                del redactor
                self.assertEqual(profile.path, str(supply.PRODUCTION_SIGNING_PROFILE_PATH))
                self.assertEqual(public_key, PUBLIC_KEY)
                self.assertEqual(repository_pattern, f"{IMAGE_REPOSITORY}*")
                self.assertEqual(source_hashes, evidence.source_hashes)
                return bundle

            def fake_probe(
                _options: gate.GateOptions,
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
                del image, run_id, redactor
                self.assertEqual(namespace, "synara-admission")
                self.assertTrue(image_pull_secret_name)
                return {
                    "case": case_id,
                    "resourceKind": resource_kind,
                    "status": "admitted" if expect_allowed else "denied",
                }

            with (
                mock.patch.object(gate, "prepare_secret_inputs", side_effect=fake_prepare),
                mock.patch.object(
                    gate,
                    "verify_kyverno_runtime",
                    return_value=kyverno_runtime(),
                ),
                mock.patch.object(
                    gate,
                    "verify_vault_cluster",
                    return_value={
                        "kubernetes": {"podCount": 3, "readyPods": 3, "runningPods": 3},
                        "vault": {
                            "principal": "auth/approle/role/synara-worker-release-signer",
                            "keyReference": supply.VAULT_TRANSIT_KEY_REFERENCE,
                            "keyType": "ecdsa-p256",
                            "transitAuditRequestId": "request-1",
                            "policyHash": "d" * 64,
                            "auditDeviceCount": 2,
                            "identities": {
                                "signer": {
                                    "roleName": evidence.signer_identity["roleName"],
                                    "tokenType": evidence.signer_identity["type"],
                                    "orphan": evidence.signer_identity["orphan"],
                                    "policyHash": evidence.signer_identity["policiesSha256"],
                                    "registryIdentitySha256": evidence.signer_identity[
                                        "identitySha256"
                                    ],
                                }
                            },
                        },
                    },
                ),
                mock.patch.object(
                    gate,
                    "export_vault_public_key",
                    return_value={
                        "path": public_key_path,
                        "publicKeySha256": gate.sha256_text(PUBLIC_KEY),
                        "keyReference": supply.VAULT_TRANSIT_KEY_REFERENCE,
                    },
                ),
                mock.patch.object(
                    gate,
                    "verify_tls_registry",
                    return_value=(
                        object(),
                        {
                            "registryHost": "registry.example.test",
                            "tlsCertificateSha256": "e" * 64,
                            "basicAuthEnforced": True,
                            "principalSha256": gate.sha256_text("registry-user"),
                            "principalEnvironment": "REGISTRY_USERNAME",
                            "passwordEnvironment": "REGISTRY_PASSWORD",
                            "caCertificateEnvironment": "REGISTRY_CA_CERT",
                        },
                    ),
                ),
                mock.patch.object(
                    gate,
                    "resolve_registry_digest_for_tag",
                    return_value=TAG_DRIFT_DIGEST,
                ),
                mock.patch.object(
                    gate,
                    "verify_positive_signature",
                    return_value={
                        "reference": f"{IMAGE_REPOSITORY}@{DIGEST}",
                        "matchingSignatureCount": 1,
                        "rekorEntries": [{"uuid": "123e4567-e89b-12d3-a456-426614174000", "index": 7}],
                        "verificationPayloadSha256": "f" * 64,
                    },
                ),
                mock.patch.object(gate, "verify_existing_admission_resources", side_effect=fake_verify_existing),
                mock.patch.object(
                    gate,
                    "verify_live_cluster_policy",
                    return_value={
                        "name": str(bundle.names.cluster_policy),
                        "documentSha256": "1" * 64,
                        "specSha256": "2" * 64,
                        "autogenControllers": list(gate.REQUIRED_AUTOGEN_CONTROLLERS),
                        "autogenControllerCount": len(gate.REQUIRED_AUTOGEN_CONTROLLERS),
                        "autogenControllersAnnotationSha256": "3" * 64,
                    },
                ),
                mock.patch.object(gate, "_apply_resource_documents"),
                mock.patch.object(
                    gate,
                    "cleanup_owned_resources",
                    return_value={
                        "ownedResourcesRemoved": [
                            "secret:synara-admission:synara-registry-probe-pull-test",
                        ],
                        "kyvernoResourcesRestored": [],
                        "exactOwnerCleanup": True,
                        "broadCleanupUsed": False,
                    },
                ),
                mock.patch.object(gate, "run_admission_probe", side_effect=fake_probe),
                contextlib.redirect_stdout(io.StringIO()),
            ):
                exit_code = gate.run_vault_kms_admission_gate(
                    options,
                    repository_state=lambda _root: {"gitSha": GIT_SHA, "worktreeDirty": False},
                    configuration_loader=lambda *_args, **_kwargs: configuration,
                    registry_release_loader=lambda _path: evidence,
                )

            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 0)
        self.assertEqual(report["status"], "pass")
        self.assertEqual(report["source"]["gitSha"], GIT_SHA)
        self.assertEqual(report["source"]["registryReleaseGate"]["gitSha"], GIT_SHA)
        self.assertEqual(report["admission"]["mode"], "verify-existing")
        self.assertEqual(
            report["registry"]["runtimeEvidence"]["tlsPeerCertificateSha256"],
            "e" * 64,
        )
        self.assertEqual(
            report["admission"]["clusterPolicy"]["autogenControllers"],
            list(gate.REQUIRED_AUTOGEN_CONTROLLERS),
        )
        self.assertEqual(
            sorted(report["admission"]["controllerProbes"]),
            ["cronjob", "deployment", "job", "statefulset"],
        )
        self.assertEqual(report["security"]["outputSecretScan"]["findings"], [])
        serialized = json.dumps(report, sort_keys=True)
        self.assertNotIn("vault-token", serialized)
        self.assertNotIn("registry-user", serialized)
        self.assertNotIn("registry-password", serialized)

    def test_fails_before_cluster_checks_when_release_report_git_sha_mismatches(self) -> None:
        configuration = production_configuration()
        evidence = release_evidence(git_sha="b" * 40)
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory) / "gate-output"
            options = gate_options(output_dir)
            with (
                mock.patch.object(gate, "verify_kyverno_runtime") as verify_kyverno_runtime,
                contextlib.redirect_stdout(io.StringIO()),
            ):
                exit_code = gate.run_vault_kms_admission_gate(
                    options,
                    repository_state=lambda _root: {"gitSha": GIT_SHA, "worktreeDirty": False},
                    configuration_loader=lambda *_args, **_kwargs: configuration,
                    registry_release_loader=lambda _path: evidence,
                )
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["status"], "fail")
        self.assertFalse(verify_kyverno_runtime.called)
        self.assertEqual(
            report["errors"][0]["code"],
            "release.vault_kms_registry_release_gate_invalid",
        )
        self.assertEqual(report["errors"][0]["evidence"]["expectedGitSha"], GIT_SHA)
        self.assertEqual(report["errors"][0]["evidence"]["actualGitSha"], "b" * 40)

    def test_rejects_non_empty_output_directory(self) -> None:
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stderr(io.StringIO()) as stderr:
            output_dir = pathlib.Path(directory) / "gate-output"
            output_dir.mkdir(parents=True)
            (output_dir / "existing.txt").write_text("occupied\n", encoding="utf-8")

            exit_code = gate.run_vault_kms_admission_gate(
                gate_options(output_dir),
                repository_state=lambda _root: {"gitSha": GIT_SHA, "worktreeDirty": False},
                configuration_loader=lambda *_args, **_kwargs: production_configuration(),
                registry_release_loader=lambda _path: release_evidence(),
            )

        self.assertEqual(exit_code, 2)
        self.assertIn("output directory must be empty or absent", stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
