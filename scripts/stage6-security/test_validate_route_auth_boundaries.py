from __future__ import annotations

import json
import pathlib
import subprocess
import sys
import tempfile
import unittest


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = pathlib.Path(__file__).with_name("validate_route_auth_boundaries.py")
SOURCE = REPO_ROOT / "services/control-plane/internal/httpapi/server.go"


class ValidateRouteAuthBoundariesTest(unittest.TestCase):
    def command(self, source: pathlib.Path = SOURCE) -> list[str]:
        return [
            sys.executable,
            str(SCRIPT),
            "--repository-root",
            str(REPO_ROOT),
            "--source",
            str(source),
        ]

    def mutated_source(self, transform) -> tuple[tempfile.TemporaryDirectory[str], pathlib.Path]:
        temporary = tempfile.TemporaryDirectory()
        path = pathlib.Path(temporary.name) / "server.go"
        path.write_text(transform(SOURCE.read_text(encoding="utf-8")), encoding="utf-8")
        return temporary, path

    def test_checked_in_routes_have_classified_outer_authentication(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(result.stdout)
        self.assertEqual(receipt["assessment"], "route-auth-boundaries-validated-not-tenant-audit-passed")
        self.assertGreaterEqual(receipt["routes"]["explicitTenantRoutes"], 100)
        self.assertEqual(receipt["routes"]["byBoundary"]["platform-signature"], 1)
        self.assertNotIn("billing-provider-signature", receipt["routes"]["byBoundary"])
        self.assertEqual(receipt["routes"]["byBoundary"]["desktop-enrollment-token"], 1)

    def test_rejects_payment_route_in_internal_self_hosted_runtime(self) -> None:
        temporary, path = self.mutated_source(
            lambda source: source.replace(
                'mux.HandleFunc("GET /health", server.health)',
                'mux.HandleFunc("GET /health", server.health)\n\tmux.Handle("POST /v1/tenants/{tenantID}/commercial-billing/checkout", server.requireAuth(http.HandlerFunc(server.createCommercialBillingCheckout)))',
            )
        )
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("forbidden in the internal-self-hosted product runtime", result.stderr)

    def test_rejects_payment_path_variant_but_allows_internal_cost_accounting_invoice_import(self) -> None:
        temporary, path = self.mutated_source(
            lambda source: source.replace(
                'mux.HandleFunc("GET /health", server.health)',
                'mux.HandleFunc("GET /health", server.health)\n\tmux.Handle("POST /v1/tenants/{tenantID}/billing/portal-session", server.requireAuth(http.HandlerFunc(server.health)))',
            )
        )
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("forbidden in the internal-self-hosted product runtime", result.stderr)

        source = SOURCE.read_text(encoding="utf-8")
        self.assertIn("/cost-accounting/imports/{provider}/{externalImportID}", source)
        self.assertNotIn("/cost-accounting/checkout", source)

    def test_rejects_tenant_route_without_login_authentication(self) -> None:
        temporary, path = self.mutated_source(
            lambda source: source.replace(
                'mux.Handle("GET /v1/tenants/{tenantID}", server.requireAuth(http.HandlerFunc(server.getTenant)))',
                'mux.HandleFunc("GET /v1/tenants/{tenantID}", server.getTenant)',
            )
        )
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("missing requireAuth", result.stderr)

    def test_rejects_new_unclassified_public_route(self) -> None:
        temporary, path = self.mutated_source(
            lambda source: source.replace(
                'mux.HandleFunc("GET /health", server.health)',
                'mux.HandleFunc("GET /health", server.health)\n\tmux.HandleFunc("GET /v1/debug", server.health)',
            )
        )
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("missing requireAuth", result.stderr)

    def test_rejects_desktop_enrollment_handler_drift(self) -> None:
        temporary, path = self.mutated_source(
            lambda source: source.replace(
                'mux.HandleFunc("POST /v1/desktop-enrollments/redeem", server.redeemDesktopEnrollment)',
                'mux.HandleFunc("POST /v1/desktop-enrollments/redeem", server.health)',
            )
        )
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("one-time handle verifier", result.stderr)

    def test_rejects_artifact_content_handler_drift(self) -> None:
        temporary, path = self.mutated_source(
            lambda source: source.replace(
                'mux.HandleFunc("GET /v1/artifact-content/{artifactID}", server.downloadArtifactContent)',
                'mux.HandleFunc("GET /v1/artifact-content/{artifactID}", server.getArtifact)',
            )
        )
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("scoped content-token verifier", result.stderr)


if __name__ == "__main__":
    unittest.main()
