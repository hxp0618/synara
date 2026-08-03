#!/usr/bin/env python3
"""Validate fail-closed enterprise OTLP wiring in the Kubernetes base."""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys


SCHEMA_VERSION = "synara.stage6-observability-deployment.v1"
ASSESSMENT = "enterprise-otel-deployment-wiring-validated-not-collector-accepted"


class DeploymentBoundaryError(Exception):
    pass


def read_text(path: pathlib.Path) -> str:
    try:
        return path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as error:
        raise DeploymentBoundaryError(f"could not read {path}") from error


def require_fragment(source: str, fragment: str, label: str) -> None:
    if fragment not in source:
        raise DeploymentBoundaryError(f"{label} is missing required OTLP wiring: {fragment}")


def require_safe_config_default(source: str, key: str, expected: str, source_label: str = "ConfigMap") -> None:
    pattern = re.compile(rf"^  {re.escape(key)}:\s*(.*?)\s*$", re.MULTILINE)
    match = pattern.search(source)
    if match is None:
        raise DeploymentBoundaryError(f"{source_label} is missing {key}")
    if match.group(1) != expected:
        raise DeploymentBoundaryError(f"{source_label} {key} must default to {expected}")


def require_config_reference(deployment: str, environment: str, key: str) -> None:
    fragment = (
        f"- name: {environment}\n"
        "              valueFrom:\n"
        "                configMapKeyRef:\n"
        "                  name: synara-control-plane-config\n"
        f"                  key: {key}"
    )
    require_fragment(deployment, fragment, "Deployment")


def require_literal(deployment: str, environment: str, value: str) -> None:
    fragment = f"- name: {environment}\n              value: {value}"
    require_fragment(deployment, fragment, "Deployment")


def validate_repository(repository_root: pathlib.Path) -> dict[str, object]:
    deploy_root = repository_root / "deploy/kubernetes"
    deployment = read_text(deploy_root / "deployment.yaml")
    config = read_text(deploy_root / "config.example.yaml")
    secret = read_text(deploy_root / "secret.example.yaml")
    acceptance = read_text(deploy_root / "acceptance.sh")
    worker_example = read_text(deploy_root / "worker-observability.example.yaml")
    sandbox_template = read_text(deploy_root / "sandbox-operator/standard.example.yaml")
    remote_worker_example = read_text(repository_root / "deploy/worker/observability.env.example")
    execution_target_root = repository_root / "services/control-plane/internal/executiontargets"
    worker_pod_source = read_text(execution_target_root / "kubernetes_pod_spec.go")
    workload_identity_source = read_text(execution_target_root / "kubernetes_workload_identity.go")
    sandbox_client_source = read_text(execution_target_root / "kubernetes_sandbox_client.go")
    sandbox_materializer_source = read_text(execution_target_root / "kubernetes_sandbox_materializer.go")
    agentd_observability_source = read_text(
        repository_root / "services/control-plane/internal/agentd/observability_environment.go"
    )
    docker_source = read_text(execution_target_root / "docker_reconciler.go")
    ssh_source = read_text(execution_target_root / "ssh_provisioner.go")
    tracing_source = read_text(repository_root / "services/control-plane/internal/tracing/tracing.go")

    safe_defaults = {
        "otel-exporter-otlp-endpoint": '""',
        "otel-exporter-otlp-protocol": "http/protobuf",
        "otel-exporter-otlp-certificate": '""',
        "otel-trace-sample-ratio": '"0.1"',
        "otel-collector-region": '""',
        "otel-trace-retention-days": '"30"',
    }
    for key, expected in safe_defaults.items():
        require_safe_config_default(config, key, expected)
    for key in ("docker-worker-observability-root", "ssh-worker-observability-root"):
        require_safe_config_default(config, key, '""')

    config_references = {
        "OTEL_EXPORTER_OTLP_ENDPOINT": "otel-exporter-otlp-endpoint",
        "OTEL_EXPORTER_OTLP_PROTOCOL": "otel-exporter-otlp-protocol",
        "OTEL_EXPORTER_OTLP_CERTIFICATE": "otel-exporter-otlp-certificate",
        "SYNARA_OTEL_TRACE_SAMPLE_RATIO": "otel-trace-sample-ratio",
        "SYNARA_OTEL_COLLECTOR_REGION": "otel-collector-region",
        "SYNARA_OTEL_TRACE_RETENTION_DAYS": "otel-trace-retention-days",
    }
    for environment, key in config_references.items():
        require_config_reference(deployment, environment, key)
    for environment, key in {
        "SYNARA_DOCKER_WORKER_OBSERVABILITY_ROOT": "docker-worker-observability-root",
        "SYNARA_SSH_WORKER_OBSERVABILITY_ROOT": "ssh-worker-observability-root",
    }.items():
        require_config_reference(deployment, environment, key)

    require_literal(
        deployment,
        "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
        "/var/run/secrets/synara/otel/client.crt",
    )
    require_literal(
        deployment,
        "OTEL_EXPORTER_OTLP_CLIENT_KEY",
        "/var/run/secrets/synara/otel/client.key",
    )
    for fragment in (
        "- name: otel-client-identity\n              mountPath: /var/run/secrets/synara/otel\n              readOnly: true",
        "- name: otel-client-identity\n          secret:\n            secretName: synara-control-plane-secrets\n            defaultMode: 0440",
        "- key: otel-client-certificate\n                path: client.crt",
        "- key: otel-client-key\n                path: client.key",
    ):
        require_fragment(deployment, fragment, "Deployment")

    for key in ("otel-client-certificate", "otel-client-key"):
        require_safe_config_default(secret, key, '""', "Secret")
        if re.search(rf"^  {re.escape(key)}:\s*\|", secret, re.MULTILINE):
            raise DeploymentBoundaryError(f"example Secret must not embed {key} PEM data")

    for forbidden in (
        "OTEL_EXPORTER_OTLP_HEADERS",
        "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
        "OTEL_EXPORTER_OTLP_INSECURE",
        "OTEL_EXPORTER_OTLP_TRACES_INSECURE",
    ):
        if forbidden in deployment or forbidden in config:
            raise DeploymentBoundaryError(f"Kubernetes base must not expose {forbidden}")

    for fragment in (
        "--from-literal=otel-exporter-otlp-endpoint=",
        "--from-literal=otel-exporter-otlp-protocol=http/protobuf",
        "--from-literal=otel-exporter-otlp-certificate=",
        "--from-literal=otel-trace-sample-ratio=0.1",
        "--from-literal=otel-collector-region=",
        "--from-literal=otel-trace-retention-days=30",
        "--from-literal=otel-client-certificate=",
        "--from-literal=otel-client-key=",
        "--from-literal=docker-worker-observability-root=",
        "--from-literal=ssh-worker-observability-root=",
    ):
        require_fragment(acceptance, fragment, "Kubernetes acceptance harness")

    for key, expected in safe_defaults.items():
        worker_key = {
            "otel-exporter-otlp-endpoint": "OTEL_EXPORTER_OTLP_ENDPOINT",
            "otel-exporter-otlp-protocol": "OTEL_EXPORTER_OTLP_PROTOCOL",
            "otel-exporter-otlp-certificate": "OTEL_EXPORTER_OTLP_CERTIFICATE",
            "otel-trace-sample-ratio": "SYNARA_OTEL_TRACE_SAMPLE_RATIO",
            "otel-collector-region": "SYNARA_OTEL_COLLECTOR_REGION",
            "otel-trace-retention-days": "SYNARA_OTEL_TRACE_RETENTION_DAYS",
        }[key]
        require_safe_config_default(worker_example, worker_key, expected, "Worker observability example")
    require_fragment(
        worker_example,
        "name: synara-agentd-observability-config-REPLACE_TARGET_UUID",
        "Worker observability example",
    )
    for forbidden in (
        "kind: Secret",
        "synara-agentd-observability-mtls-",
        "OTEL_EXPORTER_OTLP_HEADERS",
        "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
        "OTEL_EXPORTER_OTLP_CLIENT_KEY",
        "OTEL_EXPORTER_OTLP_INSECURE",
    ):
        if forbidden in worker_example:
            raise DeploymentBoundaryError(f"Worker observability example must not expose {forbidden}")

    for fragment in (
        'kubernetesObservabilityConfigMapPrefix = "synara-agentd-observability-config-"',
        "func kubernetesObservabilityEnvironment(targetID uuid.UUID) []any",
        "environment = append(environment, kubernetesObservabilityEnvironment(target.ID)...)",
        "kubernetesForbiddenWorkerObservabilityEnvironment",
    ):
        combined = worker_pod_source + "\n" + read_text(execution_target_root / "kubernetes_reconciler.go")
        require_fragment(combined, fragment, "generated Kubernetes Worker Pod")
    for fragment in (
        "kubernetesObservabilityConfigMapName(targetID)",
        "kubernetesForbiddenWorkerObservabilityEnvironment[name]",
    ):
        require_fragment(workload_identity_source, fragment, "Kubernetes workload identity verifier")
    for fragment in (
        "func kubernetesSandboxTemplateObservabilityReady(",
        "observation.TemplateObservabilityReady = kubernetesSandboxTemplateObservabilityReady(",
        "observedAcceptance.TemplateObservabilityReady",
    ):
        require_fragment(
            sandbox_client_source + "\n" + sandbox_materializer_source,
            fragment,
            "sandbox-operator acceptance",
        )
    for fragment in (
        "name: synara-agentd-observability-config-REPLACE_TARGET_UUID",
        "optional: true",
    ):
        require_fragment(sandbox_template, fragment, "sandbox-operator standard template")
    for forbidden in (
        "synara-agentd-observability-mtls-",
        "OTEL_EXPORTER_OTLP_HEADERS",
        "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
        "OTEL_EXPORTER_OTLP_CLIENT_KEY",
        "OTEL_EXPORTER_OTLP_INSECURE",
    ):
        if forbidden in sandbox_template:
            raise DeploymentBoundaryError(f"sandbox-operator standard template must not expose {forbidden}")

    for fragment in (
        "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.com/v1/traces",
        "OTEL_EXPORTER_OTLP_CERTIFICATE=",
        "SYNARA_OTEL_COLLECTOR_REGION=REPLACE_REGION",
        "SYNARA_OTEL_TRACE_RETENTION_DAYS=30",
    ):
        require_fragment(remote_worker_example, fragment, "remote Worker observability example")
    for forbidden in (
        "OTEL_EXPORTER_OTLP_HEADERS",
        "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE=",
        "OTEL_EXPORTER_OTLP_CLIENT_KEY=",
        "OTEL_EXPORTER_OTLP_INSECURE",
    ):
        if forbidden in remote_worker_example:
            raise DeploymentBoundaryError(f"remote Worker observability example must not expose {forbidden}")
    for fragment in (
        "func LoadObservabilityEnvironment(cfg Config) error",
        "validateObservabilityCertificatePaths",
        "agentd observability environment path must not traverse symlinks",
        "agentd observability environment conflicts with process",
        '"OTEL_EXPORTER_OTLP_CLIENT_KEY"',
    ):
        require_fragment(agentd_observability_source, fragment, "agentd observability file loader")
    for fragment in (
        "SYNARA_AGENTD_OBSERVABILITY_ENV_FILE=",
        'observabilitySource+":"+observabilityDestination+":ro"',
        "filepath.Join(root, target.ID.String())",
    ):
        require_fragment(docker_source, fragment, "managed Docker observability wiring")
    for fragment in (
        'pathpkg.Join(root, target.ID.String(), "observability.env")',
        '"SYNARA_AGENTD_OBSERVABILITY_ENV_FILE"',
    ):
        require_fragment(ssh_source, fragment, "managed SSH observability wiring")
    for fragment in (
        'ExportPolicyEnterpriseWorker ExportPolicy = "enterprise-worker"',
        "enterprise Worker OTLP trace export forbids client identity files",
        "enterprise Worker HTTP OTLP trace export requires an explicit loopback relay without TLS files",
    ):
        require_fragment(tracing_source, fragment, "enterprise Worker exporter policy")

    return {
        "schemaVersion": SCHEMA_VERSION,
        "deployment": "deploy/kubernetes/deployment.yaml",
        "exportDefault": "disabled-empty-endpoint",
        "transport": "https-otlp-http-mtls-required-by-enterprise-runtime",
        "clientIdentity": "read-only-secret-volume",
        "acceptanceHarness": "disabled-export-fixture-complete",
        "workerExport": "operator-owned-target-scoped-credentialless-config",
        "remoteWorkerExport": "operator-root-target-scoped-credentialless-readonly-file",
        "dataPolicy": {
            "collectorRegion": "required-when-enabled",
            "retentionDays": "1-90-required-when-enabled",
        },
        "assessment": ASSESSMENT,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", default=".")
    args = parser.parse_args()
    try:
        receipt = validate_repository(pathlib.Path(args.repository_root).resolve())
    except DeploymentBoundaryError as error:
        print(f"observability deployment validation failed: {error}", file=sys.stderr)
        return 2
    print(json.dumps(receipt, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
