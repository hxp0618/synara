#!/usr/bin/env python3
"""Fail closed when payment capability re-enters the internal-self-hosted runtime or product surface."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys


class BoundaryError(Exception):
    pass


FORBIDDEN_PATHS = (
    "packages/enterprise-ui/src/TenantCommercialBillingSection.tsx",
    "services/control-plane/internal/commercialbilling",
    "services/control-plane/internal/httpapi/commercial_billing_api.go",
    "services/control-plane/internal/httpapi/billing_exercise_governance_api.go",
    "docs/contracts/commercial-subscription-billing-v1.md",
    "docs/runbooks/stripe-subscription-billing.md",
    "scripts/stage6-billing",
    "services/control-plane/internal/billingexercisegovernance",
    "services/control-plane/internal/testsupport/stage6billing",
)

SCANNED_FILES = {
    "packages/control-plane-client/src/index.ts": (
        "commercial-billing",
        "ControlPlaneCommercialBilling",
        "Stage6BillingExercise",
        "checkout.stripe.com",
        "billing.stripe.com",
        "billing_admin",
        "assignPlatformTenantPlan",
        "/plan-assignment",
        "planAssignmentVersion",
        "planAssignment:",
        "trialExpiresAt",
        'planCode:',
        '"trialing"',
        "resolveControlPlaneStatusPageURL",
        "bindPlatformStage6IncidentStatusPage",
        "addPlatformStage6IncidentPublicUpdate",
        "/status-page",
        "customerImpactSummary:",
        "publicImpact:",
        "statusPageIncidentReference:",
    ),
    "services/control-plane/internal/httpapi/server.go": (
        "commercialbilling",
        "commercialBilling",
        "billingExerciseGovernance",
        "/commercial-billing",
        "/billing-exercises",
        "/billing/",
        "/plan-assignment",
        "assignPlatformTenantPlan",
        "/status-page",
        "addPlatformIncidentPublicUpdate",
        "bindPlatformIncidentStatusPage",
    ),
    "services/control-plane/internal/authorization/permissions.go": (
        "billing_admin",
        "billing.manage",
        "BillingManage",
    ),
    "services/control-plane/internal/httpapi/execution_targets_api.go": ("commercialBilling",),
    "services/control-plane/internal/observability/metrics.go": (
        "commercialBilling",
        "commercial-billing-reconciliation",
    ),
    "services/control-plane/go.mod": ("stripe-go",),
    "deploy/saas/.env.example": (
        "SYNARA_COMMERCIAL_BILLING",
        "SYNARA_STRIPE",
        "SYNARA_BILLING_",
        "synara-saas",
    ),
    "deploy/saas/docker-compose.yml": (
        "SYNARA_COMMERCIAL_BILLING",
        "SYNARA_STRIPE",
        "SYNARA_BILLING_",
        "synara-saas",
    ),
    "deploy/kubernetes/config.example.yaml": ("commercial-billing", "stripe"),
    "deploy/kubernetes/monitoring/README.md": ("Status Page URLs",),
    "deploy/kubernetes/deployment.yaml": (
        "SYNARA_COMMERCIAL_BILLING",
        "SYNARA_STRIPE",
        "SYNARA_BILLING_",
    ),
    "deploy/kubernetes/monitoring/prometheus-rules.yaml": ("commercial_billing", "CommercialBilling"),
    "apps/server/src/controlPlaneProxy.ts": ("SaaS control plane",),
    "apps/web/src/components/ControlPlaneGate.tsx": ("local SaaS", "standard internal Plan"),
    "apps/web/src/components/settings/DesktopSettingsPanels.tsx": (
        "SaaS device",
        "SaaS endpoint",
        "Synara SaaS",
    ),
    "apps/web/src/settingsNavigation.ts": ("SaaS connection", "Synara SaaS"),
    "apps/web/src/settingsSearchIndex.ts": ("Usage and billing",),
    "packages/enterprise-ui/src/TenantUsageSettingsSection.tsx": (
        "Plan usage",
        "no Plan limit",
        "Upgrade the Plan",
        "invoice",
        "payment",
        "checkout",
        "stripe",
        "billing",
    ),
    "apps/web/src/components/chat/environment/EnvironmentSessionUsageSection.tsx": (
        "Platform charges",
        "invoice",
        "payment",
        "checkout",
        "stripe",
        "billing",
    ),
    "packages/enterprise-ui/src/TenantOrganizationSettingsPanel.tsx": (
        "activeTenant.planCode} plan",
    ),
    "packages/enterprise-ui/src/TenantStatusLifecycleSettingsSection.tsx": (
        "trial expires",
        '"trialing"',
        ".trialExpiresAt",
    ),
    "apps/admin/src/platform/PlatformSubscriptionManagement.tsx": (
        "Manage internal Plan entitlement",
        'label="Plan"',
        "Trial end",
        "assignPlatformTenantPlan",
        ".planAssignment",
        '"trialing"',
        "payment",
        "invoice",
        "checkout",
        "stripe",
        "billing",
    ),
    "apps/admin/src/platform/PlatformInternalCostGovernance.tsx": (
        "payment",
        "invoice",
        "checkout",
        "stripe",
        "billing",
    ),
    "apps/admin/src/platform/navigation.ts": ("subscriptions",),
    "docs/contracts/enterprise-operations-ui-v1.md": (
        "versioned Subscription management",
        "free Plan",
        "Billing roles",
        "all 27 positive results",
    ),
    "apps/admin/src/platform/PlatformTenantOperations.tsx": (
        "Filter by Plan",
        "All Plans",
        "Plan entitlement",
        'data-label="Plan"',
        ".planCode",
        ".planAssignment",
        '"trialing"',
    ),
    "apps/admin/src/platform/PlatformTenantProvisioning.tsx": (
        'label="Plan"',
        "planCode",
        "trialExpiresAt",
        '"trialing"',
    ),
    "apps/admin/src/platform/PlatformIncidentGovernance.tsx": (
        "Bind external Status Page incident",
        "Record externally published update",
        "Public timeline",
        "addPlatformStage6IncidentPublicUpdate",
        "bindPlatformStage6IncidentStatusPage",
        ".customerImpactSummary",
        ".publicImpact",
        ".statusPageIncidentReference",
    ),
    "services/control-plane/internal/tenancy/models.go": (
        'json:"planCode"',
        'json:"trialExpiresAt"',
    ),
    "services/control-plane/internal/identity/service.go": (
        'json:"planCode"',
        'json:"trialExpiresAt"',
    ),
    "services/control-plane/internal/supportaccess/service.go": ('json:"planCode"',),
    "services/control-plane/internal/usage/tenant_usage.go": (
        "Upgrade the Plan",
        "upgrade the Plan",
        'json:"planAssignmentVersion"',
    ),
    "services/control-plane/internal/usage/cost_allocation.go": ('json:"planAssignmentVersion"',),
    "services/control-plane/internal/desktopenrollment/rotation.go": ("Synara SaaS",),
    "services/control-plane/README.md": ("Synara SaaS control plane", "owns SaaS identity"),
    "docs/runbooks/enterprise-release-governance.md": ("Enterprise SaaS", "Stage 6 SaaS"),
    "services/control-plane/internal/httpapi/billing_api.go": (),
    "services/control-plane/internal/billing/estimate_sweeper.go": (),
    "services/control-plane/internal/billing/management.go": (),
    "services/control-plane/internal/billing/scheduler.go": (),
    "services/control-plane/internal/billing/service.go": (),
    "services/control-plane/internal/billing/shared_actual_allocation.go": (),
    "services/control-plane/internal/billing/shared_allocation.go": (),
    "services/control-plane/internal/billing/shared_management.go": (),
}

ACTIVE_COST_CONTRACT_FILES = tuple(
    relative
    for relative in SCANNED_FILES
    if relative.startswith("services/control-plane/internal/billing/")
    or relative == "services/control-plane/internal/httpapi/billing_api.go"
)

# Historical table names, Go package names, and SQL fragments remain compatible. Exact
# quoted tokens are the externally observable Problem Code, audit action, and resource
# type contracts written by current runtime paths and must use the product vocabulary.
LEGACY_ACTIVE_COST_CONTRACT = re.compile(
    r'"(?:invalid_)?billing_[a-z0-9_]+"|"billing\.[a-z0-9_.]+"'
)

# The migration history and SQLite safety layer retain a small set of typed
# compatibility models so an upgraded installation can still read and
# reject historical payment rows. Those types must never become a dependency
# of a current product service. Keep the allow-list narrow and scan every
# other Control Plane Go source file for both type and table markers.
LEGACY_PAYMENT_RUNTIME_MARKER = re.compile(
    r"(?:\bCommercialBillingCheckoutSession\b|\bCommercialBillingProviderEvent\b|"
    r"\bStage6BillingExercise(?:Approval)?\b|"
    r"\bcommercial_billing_(?:checkout|provider_events)(?:_|\b)|"
    r"\bstage6_billing_exercise(?:s|_approvals)?(?:_|\b)|\bbilling_exercise\.)"
)
LEGACY_PAYMENT_COMPATIBILITY_FILES = frozenset(
    {
        "services/control-plane/internal/database/billing_exercise_governance_sqlite.go",
        "services/control-plane/internal/database/commercial_billing_sqlite.go",
        "services/control-plane/internal/database/governance_authority_sqlite.go",
        "services/control-plane/internal/database/internal_cost_governance_sqlite.go",
        "services/control-plane/internal/database/release_governance_sqlite.go",
        "services/control-plane/internal/persistence/billing_exercise_governance_models.go",
        "services/control-plane/internal/persistence/commercial_billing_models.go",
        "services/control-plane/internal/persistence/schema.go",
    }
)

REQUIRED_CONFIG_MARKERS = (
    "SYNARA_COMMERCIAL_BILLING_PROVIDER",
    "SYNARA_STRIPE_SECRET_KEY",
    "unsupported by the internal-self-hosted product",
    "SYNARA_BILLING_BLOB_SOURCE",
    "SYNARA_COST_ACCOUNTING_BLOB_SOURCE",
    "is retired; use",
    "SYNARA_PUBLIC_STATUS_PAGE_URL",
    "SYNARA_INTERNAL_STATUS_BOARD_URL",
    "SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL",
    "SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY",
    "internal incident publisher is not configured",
    "internal incident communications",
)


def validate_legacy_payment_runtime_boundary(root: pathlib.Path) -> dict[str, str]:
    """Allow historical compatibility code only at explicit safety seams."""

    internal_root = root / "services/control-plane/internal"
    if not internal_root.is_dir():
        return {}
    compatibility_digests: dict[str, str] = {}
    for path in sorted(internal_root.rglob("*.go")):
        if path.name.endswith("_test.go") or path.is_symlink():
            continue
        relative = path.relative_to(root).as_posix()
        data = path.read_bytes()
        text = data.decode("utf-8")
        if LEGACY_PAYMENT_RUNTIME_MARKER.search(text) is None:
            continue
        if relative not in LEGACY_PAYMENT_COMPATIBILITY_FILES:
            raise BoundaryError(
                "legacy payment compatibility marker is reachable from active runtime source: "
                + relative
            )
        compatibility_digests[relative] = "sha256:" + hashlib.sha256(data).hexdigest()
    return dict(sorted(compatibility_digests.items()))


def validate(root: pathlib.Path) -> dict[str, object]:
    root = root.resolve()
    if not root.is_dir() or root.is_symlink():
        raise BoundaryError("repository root must be a regular non-symlink directory")
    for relative in FORBIDDEN_PATHS:
        if (root / relative).exists():
            raise BoundaryError(f"forbidden payment product path exists: {relative}")

    digests: dict[str, str] = {}
    for relative, markers in SCANNED_FILES.items():
        path = root / relative
        if not path.is_file() or path.is_symlink():
            raise BoundaryError(f"required product-boundary source is missing or unsafe: {relative}")
        data = path.read_bytes()
        text = data.decode("utf-8")
        for marker in markers:
            if marker.lower() in text.lower():
                raise BoundaryError(f"product-boundary marker {marker!r} is forbidden in {relative}")
        if relative in ACTIVE_COST_CONTRACT_FILES:
            match = LEGACY_ACTIVE_COST_CONTRACT.search(text)
            if match is not None:
                raise BoundaryError(
                    f"legacy active cost-accounting contract {match.group(0)!r} is forbidden in {relative}"
                )
        digests[relative] = "sha256:" + hashlib.sha256(data).hexdigest()

    legacy_compatibility_files = validate_legacy_payment_runtime_boundary(root)

    config_path = root / "services/control-plane/internal/config/config.go"
    if not config_path.is_file() or config_path.is_symlink():
        raise BoundaryError("internal self-hosted configuration source is missing or unsafe")
    config_text = config_path.read_text(encoding="utf-8")
    for marker in REQUIRED_CONFIG_MARKERS:
        if marker not in config_text:
            raise BoundaryError(f"configuration no longer rejects legacy payment marker {marker!r}")
    digests[config_path.relative_to(root).as_posix()] = "sha256:" + hashlib.sha256(
        config_path.read_bytes()
    ).hexdigest()

    return {
        "schemaVersion": "synara.internal-self-hosted-product-boundary.v1",
        "assessment": "payment-runtime-and-product-surfaces-absent",
        "forbiddenPathCount": len(FORBIDDEN_PATHS),
        "activeCostContractFileCount": len(ACTIVE_COST_CONTRACT_FILES),
        "publicEntitlementVocabulary": "entitlement-profile-evaluation",
        "scannedFiles": dict(sorted(digests.items())),
        "legacyPaymentCompatibilityFiles": legacy_compatibility_files,
        "legacyDatabaseBoundary": "migrations-and-read-only-history-excluded-from-runtime-scan",
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", default=".")
    args = parser.parse_args()
    try:
        receipt = validate(pathlib.Path(args.repository_root))
    except (BoundaryError, OSError, UnicodeDecodeError) as error:
        print(f"internal self-hosted boundary validation failed: {error}", file=sys.stderr)
        return 2
    print(json.dumps(receipt, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
