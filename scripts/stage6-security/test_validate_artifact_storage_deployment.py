from __future__ import annotations

import json
import pathlib
import shutil
import subprocess
import sys
import tempfile
import unittest


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = pathlib.Path(__file__).with_name("validate_artifact_storage_deployment.py")
FIXTURES = (
    "deploy/saas/docker-compose.yml",
    "deploy/saas/minio/bootstrap.sh",
    "deploy/saas/minio/artifact-policy.json",
    "deploy/kubernetes/deployment.yaml",
    "deploy/kubernetes/service-account.yaml",
    "deploy/kubernetes/secret.example.yaml",
    "services/control-plane/internal/config/config.go",
    "services/control-plane/internal/artifacts/s3_store.go",
)


class ValidateArtifactStorageDeploymentTest(unittest.TestCase):
    def command(self, repository_root: pathlib.Path = REPO_ROOT) -> list[str]:
        return [sys.executable, str(SCRIPT), "--repository-root", str(repository_root)]

    def mutated_repository(self, filename: str, transform) -> tuple[tempfile.TemporaryDirectory[str], pathlib.Path]:
        temporary = tempfile.TemporaryDirectory()
        root = pathlib.Path(temporary.name)
        for source_name in FIXTURES:
            source = REPO_ROOT / source_name
            destination = root / source_name
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, destination)
        path = root / filename
        path.write_text(transform(path.read_text(encoding="utf-8")), encoding="utf-8")
        return temporary, root

    def assert_rejected(self, filename: str, transform, message: str) -> None:
        temporary, root = self.mutated_repository(filename, transform)
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(root), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2, result.stdout)
        self.assertIn(message, result.stderr)

    def test_checked_in_deployment_is_fail_closed(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(result.stdout)
        self.assertEqual(receipt["compose"]["applicationIdentity"], "dedicated-non-root-minio-user")
        self.assertEqual(receipt["kubernetes"]["credentials"], "workload-identity-no-static-artifact-secret")
        self.assertEqual(receipt["runtime"]["presignMaximum"], "15m")
        self.assertEqual(receipt["assessment"], "artifact-storage-deployment-wiring-validated-not-cloud-iam-accepted")

    def test_rejects_root_credentials_in_control_plane(self) -> None:
        self.assert_rejected(
            "deploy/saas/docker-compose.yml",
            lambda source: source.replace(
                "SYNARA_ARTIFACT_ACCESS_KEY_ID: ${MINIO_ARTIFACT_USER}",
                "SYNARA_ARTIFACT_ACCESS_KEY_ID: ${MINIO_ROOT_USER}",
            ),
            "MINIO_ROOT_USER",
        )

    def test_rejects_cors_fallback(self) -> None:
        self.assert_rejected(
            "deploy/saas/docker-compose.yml",
            lambda source: source.replace(
                "${SYNARA_ARTIFACT_CORS_ALLOW_ORIGIN:?Set the exact browser origin in deploy/saas/.env}",
                "${SYNARA_ARTIFACT_CORS_ALLOW_ORIGIN:-*}",
            ),
            "CORS",
        )

    def test_rejects_unpinned_bootstrap_image(self) -> None:
        self.assert_rejected(
            "deploy/saas/docker-compose.yml",
            lambda source: source.replace("minio/mc:RELEASE.2025-04-16T18-13-26Z", "minio/mc:latest"),
            "minio/mc:RELEASE.2025-04-16T18-13-26Z",
        )

    def test_rejects_object_policy_outside_tenant_prefix(self) -> None:
        self.assert_rejected(
            "deploy/saas/minio/artifact-policy.json",
            lambda source: source.replace("/tenants/*", "/*"),
            "resources exceed",
        )

    def test_rejects_policy_admin_action(self) -> None:
        self.assert_rejected(
            "deploy/saas/minio/artifact-policy.json",
            lambda source: source.replace('"s3:GetObject"', '"s3:*"'),
            "actions exceed",
        )

    def test_rejects_kubernetes_static_artifact_secret(self) -> None:
        self.assert_rejected(
            "deploy/kubernetes/deployment.yaml",
            lambda source: source.replace(
                '            - name: AWS_EC2_METADATA_DISABLED',
                '            - name: SYNARA_ARTIFACT_ACCESS_KEY_ID\n              value: forbidden\n            - name: AWS_EC2_METADATA_DISABLED',
            ),
            "must not inject SYNARA_ARTIFACT_ACCESS_KEY_ID",
        )

    def test_rejects_relaxed_presign_limit(self) -> None:
        self.assert_rejected(
            "services/control-plane/internal/config/config.go",
            lambda source: source.replace("cfg.ArtifactPresignTTL > 15*time.Minute", "cfg.ArtifactPresignTTL > 24*time.Hour"),
            "15*time.Minute",
        )


if __name__ == "__main__":
    unittest.main()
