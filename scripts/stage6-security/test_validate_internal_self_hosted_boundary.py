from __future__ import annotations

import importlib.util
import pathlib
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_internal_self_hosted_boundary.py")
SPEC = importlib.util.spec_from_file_location("validate_internal_self_hosted_boundary", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class InternalSelfHostedBoundaryTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        for relative in MODULE.SCANNED_FILES:
            path = self.root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("internal self hosted source\n", encoding="utf-8")
        config = self.root / "services/control-plane/internal/config/config.go"
        config.parent.mkdir(parents=True, exist_ok=True)
        config.write_text("\n".join(MODULE.REQUIRED_CONFIG_MARKERS), encoding="utf-8")

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_accepts_runtime_without_payment_capability(self) -> None:
        receipt = MODULE.validate(self.root)
        self.assertEqual(receipt["assessment"], "payment-runtime-and-product-surfaces-absent")
        self.assertEqual(len(receipt["scannedFiles"]), len(MODULE.SCANNED_FILES) + 1)
        self.assertEqual(receipt["legacyPaymentCompatibilityFiles"], {})

    def test_allows_only_explicit_legacy_payment_compatibility_seams(self) -> None:
        relative = next(iter(MODULE.LEGACY_PAYMENT_COMPATIBILITY_FILES))
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("// historical compatibility lock\nbilling_exercise.finance\n", encoding="utf-8")
        receipt = MODULE.validate(self.root)
        self.assertIn(relative, receipt["legacyPaymentCompatibilityFiles"])

    def test_rejects_legacy_payment_marker_in_active_runtime_source(self) -> None:
        relative = "services/control-plane/internal/usage/payment_bridge.go"
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("package usage\nconst table = \"commercial_billing_checkout_sessions\"\n", encoding="utf-8")
        with self.assertRaisesRegex(MODULE.BoundaryError, "active runtime source"):
            MODULE.validate(self.root)

    def test_rejects_payment_product_path(self) -> None:
        path = self.root / MODULE.FORBIDDEN_PATHS[0]
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("export function Checkout() {}", encoding="utf-8")
        with self.assertRaisesRegex(MODULE.BoundaryError, "forbidden payment product path"):
            MODULE.validate(self.root)

    def test_rejects_payment_marker_in_runtime_source(self) -> None:
        relative = "packages/control-plane-client/src/index.ts"
        (self.root / relative).write_text("const path = '/commercial-billing'\n", encoding="utf-8")
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_external_saas_positioning_in_current_product_surface(self) -> None:
        relative = "apps/web/src/settingsNavigation.ts"
        (self.root / relative).write_text('const label = "SaaS connection"\n', encoding="utf-8")
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_plan_language_in_tenant_creation_copy(self) -> None:
        relative = "apps/web/src/components/ControlPlaneGate.tsx"
        (self.root / relative).write_text(
            "The Tenant starts on the standard internal Plan.\n", encoding="utf-8"
        )
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_external_plan_upgrade_positioning_in_internal_usage_surface(self) -> None:
        relative = "services/control-plane/internal/usage/tenant_usage.go"
        (self.root / relative).write_text(
            'const recommendation = "Upgrade the Plan"\n', encoding="utf-8"
        )
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_payment_vocabulary_in_shared_usage_surface(self) -> None:
        relative = "packages/enterprise-ui/src/TenantUsageSettingsSection.tsx"
        (self.root / relative).write_text('const label = "external invoice"\n', encoding="utf-8")
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_payment_vocabulary_in_session_usage_surface(self) -> None:
        relative = "apps/web/src/components/chat/environment/EnvironmentSessionUsageSection.tsx"
        (self.root / relative).write_text('const label = "Platform charges"\n', encoding="utf-8")
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_payment_vocabulary_in_platform_admin_surfaces(self) -> None:
        relative = "apps/admin/src/platform/PlatformInternalCostGovernance.tsx"
        (self.root / relative).write_text('const label = "payment provider"\n', encoding="utf-8")
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_subscription_route_in_platform_admin_navigation(self) -> None:
        relative = "apps/admin/src/platform/navigation.ts"
        (self.root / relative).write_text('const destination = "subscriptions"\n', encoding="utf-8")
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_stale_subscription_vocabulary_in_operations_contract(self) -> None:
        relative = "docs/contracts/enterprise-operations-ui-v1.md"
        (self.root / relative).write_text(
            "The contract still describes versioned Subscription management.\n", encoding="utf-8"
        )
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_legacy_plan_assignment_route_in_public_client(self) -> None:
        relative = "packages/control-plane-client/src/index.ts"
        (self.root / relative).write_text(
            "const route = '/v1/platform/tenants/id/plan-assignment'\n", encoding="utf-8"
        )
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_legacy_plan_json_field_in_session_contract(self) -> None:
        relative = "services/control-plane/internal/identity/service.go"
        (self.root / relative).write_text('PlanCode string `json:"planCode"`\n', encoding="utf-8")
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_external_status_page_product_route(self) -> None:
        relative = "services/control-plane/internal/httpapi/server.go"
        (self.root / relative).write_text(
            'mux.Handle("POST /v1/platform/incidents/{incidentID}/status-page", handler)\n',
            encoding="utf-8",
        )
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_customer_facing_incident_ui_copy(self) -> None:
        relative = "apps/admin/src/platform/PlatformIncidentGovernance.tsx"
        (self.root / relative).write_text("<h2>Bind external Status Page incident</h2>\n", encoding="utf-8")
        with self.assertRaisesRegex(MODULE.BoundaryError, "product-boundary marker"):
            MODULE.validate(self.root)

    def test_rejects_legacy_billing_problem_or_audit_contract(self) -> None:
        relative = "services/control-plane/internal/billing/management.go"
        (self.root / relative).write_text(
            'const problemCode = "billing_tariff_missing"\n'
            'const auditAction = "billing.tariff_created"\n',
            encoding="utf-8",
        )
        with self.assertRaisesRegex(MODULE.BoundaryError, "legacy active cost-accounting contract"):
            MODULE.validate(self.root)

    def test_requires_fail_closed_legacy_environment_rejection(self) -> None:
        config = self.root / "services/control-plane/internal/config/config.go"
        config.write_text("SYNARA_STRIPE_SECRET_KEY", encoding="utf-8")
        with self.assertRaisesRegex(
            MODULE.BoundaryError,
            "no longer enforces required internal-self-hosted marker",
        ):
            MODULE.validate(self.root)


if __name__ == "__main__":
    unittest.main()
