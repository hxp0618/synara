from __future__ import annotations

import json
import pathlib
import shutil
import subprocess
import sys
import tempfile
import unittest


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = pathlib.Path(__file__).with_name("validate_observability_deployment.py")


class ValidateObservabilityDeploymentTest(unittest.TestCase):
    def command(self, repository_root: pathlib.Path = REPO_ROOT) -> list[str]:
        return [sys.executable, str(SCRIPT), "--repository-root", str(repository_root)]

    def mutated_repository(self, filename: str, transform) -> tuple[tempfile.TemporaryDirectory[str], pathlib.Path]:
        temporary = tempfile.TemporaryDirectory()
        root = pathlib.Path(temporary.name)
        deploy = root / "deploy/kubernetes"
        deploy.mkdir(parents=True)
        for source in (
            "deployment.yaml",
            "config.example.yaml",
            "secret.example.yaml",
            "acceptance.sh",
            "worker-observability.example.yaml",
        ):
            shutil.copy2(REPO_ROOT / "deploy/kubernetes" / source, deploy / source)
        sandbox = deploy / "sandbox-operator"
        sandbox.mkdir()
        shutil.copy2(REPO_ROOT / "deploy/kubernetes/sandbox-operator/standard.example.yaml", sandbox / "standard.example.yaml")
        worker = root / "deploy/worker"
        worker.mkdir()
        shutil.copy2(REPO_ROOT / "deploy/worker/observability.env.example", worker / "observability.env.example")
        execution_targets = root / "services/control-plane/internal/executiontargets"
        execution_targets.mkdir(parents=True)
        for source in (
            "kubernetes_pod_spec.go",
            "kubernetes_reconciler.go",
            "kubernetes_workload_identity.go",
            "kubernetes_sandbox_client.go",
            "kubernetes_sandbox_materializer.go",
            "docker_reconciler.go",
            "ssh_provisioner.go",
        ):
            shutil.copy2(REPO_ROOT / "services/control-plane/internal/executiontargets" / source, execution_targets / source)
        agentd = root / "services/control-plane/internal/agentd"
        agentd.mkdir(parents=True)
        shutil.copy2(
            REPO_ROOT / "services/control-plane/internal/agentd/observability_environment.go",
            agentd / "observability_environment.go",
        )
        tracing = root / "services/control-plane/internal/tracing"
        tracing.mkdir(parents=True)
        shutil.copy2(REPO_ROOT / "services/control-plane/internal/tracing/tracing.go", tracing / "tracing.go")
        path = root / filename if filename.startswith(("deploy/", "services/")) else deploy / filename
        path.write_text(transform(path.read_text(encoding="utf-8")), encoding="utf-8")
        return temporary, root

    def assert_rejected(self, filename: str, transform, message: str) -> None:
        temporary, root = self.mutated_repository(filename, transform)
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(root), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2, result.stdout)
        self.assertIn(message, result.stderr)

    def test_checked_in_kubernetes_base_is_fail_closed(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(result.stdout)
        self.assertEqual(receipt["exportDefault"], "disabled-empty-endpoint")
        self.assertEqual(receipt["clientIdentity"], "read-only-secret-volume")
        self.assertEqual(receipt["workerExport"], "operator-owned-target-scoped-credentialless-config")
        self.assertEqual(receipt["remoteWorkerExport"], "operator-root-target-scoped-credentialless-readonly-file")
        self.assertEqual(receipt["assessment"], "enterprise-otel-deployment-wiring-validated-not-collector-accepted")

    def test_rejects_enabled_example_endpoint(self) -> None:
        self.assert_rejected(
            "config.example.yaml",
            lambda source: source.replace(
                'otel-exporter-otlp-endpoint: ""',
                "otel-exporter-otlp-endpoint: https://collector.example.test",
            ),
            "must default",
        )

    def test_rejects_missing_region_wiring(self) -> None:
        self.assert_rejected(
            "deployment.yaml",
            lambda source: source.replace("- name: SYNARA_OTEL_COLLECTOR_REGION", "- name: REMOVED_COLLECTOR_REGION"),
            "SYNARA_OTEL_COLLECTOR_REGION",
        )

    def test_rejects_writable_client_identity(self) -> None:
        self.assert_rejected(
            "deployment.yaml",
            lambda source: source.replace("readOnly: true", "readOnly: false", 1),
            "readOnly: true",
        )

    def test_rejects_insecure_override_surface(self) -> None:
        self.assert_rejected(
            "config.example.yaml",
            lambda source: source.replace(
                'otel-exporter-otlp-endpoint: ""',
                'otel-exporter-otlp-endpoint: ""\n  OTEL_EXPORTER_OTLP_INSECURE: "false"',
            ),
            "must not expose OTEL_EXPORTER_OTLP_INSECURE",
        )

    def test_rejects_secret_identity_omission(self) -> None:
        self.assert_rejected(
            "secret.example.yaml",
            lambda source: source.replace('  otel-client-key: ""\n', ""),
            "Secret is missing otel-client-key",
        )

    def test_rejects_incomplete_acceptance_fixture(self) -> None:
        self.assert_rejected(
            "acceptance.sh",
            lambda source: source.replace("  --from-literal=otel-client-key= \\\n", ""),
            "Kubernetes acceptance harness",
        )

    def test_rejects_enabled_worker_example_endpoint(self) -> None:
        self.assert_rejected(
            "worker-observability.example.yaml",
            lambda source: source.replace(
                'OTEL_EXPORTER_OTLP_ENDPOINT: ""',
                "OTEL_EXPORTER_OTLP_ENDPOINT: https://collector.example.test",
            ),
            "Worker observability example OTEL_EXPORTER_OTLP_ENDPOINT must default",
        )

    def test_rejects_worker_config_reference_drift(self) -> None:
        self.assert_rejected(
            "sandbox-operator/standard.example.yaml",
            lambda source: source.replace(
                "synara-agentd-observability-config-REPLACE_TARGET_UUID",
                "tenant-selected-config",
            ),
            "sandbox-operator standard template",
        )

    def test_rejects_remote_worker_header_credential(self) -> None:
        self.assert_rejected(
            "deploy/worker/observability.env.example",
            lambda source: source + "OTEL_EXPORTER_OTLP_HEADERS=authorization=secret\n",
            "must not expose OTEL_EXPORTER_OTLP_HEADERS",
        )

    def test_rejects_writable_managed_docker_bind(self) -> None:
        self.assert_rejected(
            "services/control-plane/internal/executiontargets/docker_reconciler.go",
            lambda source: source.replace(
                'observabilitySource+":"+observabilityDestination+":ro"',
                'observabilitySource+":"+observabilityDestination+":rw"',
            ),
            "managed Docker observability wiring",
        )


if __name__ == "__main__":
    unittest.main()
