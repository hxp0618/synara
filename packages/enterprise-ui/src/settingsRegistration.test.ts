import { describe, expect, it } from "vitest";

import {
  ENTERPRISE_SETTINGS_NAV_ITEMS,
  isEnterpriseSettingsSection,
  resolveEnterpriseSettingsNavItems,
  type EnterpriseSettingsCapabilities,
  type EnterpriseSettingsRuntime,
} from "./settingsRegistration";

const NO_CAPABILITIES: EnterpriseSettingsCapabilities = {
  canReadMembers: false,
  canReadIdentity: false,
  canReadServiceAccounts: false,
  canReadCredentials: false,
  canReadQuota: false,
  canReadRetention: false,
  canReadSchedulingPolicy: false,
  canReadAudit: false,
  canReadOutbox: false,
};

function runtime(overrides: Partial<EnterpriseSettingsRuntime> = {}): EnterpriseSettingsRuntime {
  return {
    availability: "available",
    authentication: "authenticated",
    hasSession: true,
    hasActiveTenant: true,
    capabilities: NO_CAPABILITIES,
    ...overrides,
  };
}

describe("enterprise settings registration", () => {
  it("keeps local, detecting, and unavailable hosts free of Control Plane-only destinations", () => {
    for (const availability of ["local", "detecting", "unavailable"] as const) {
      expect(resolveEnterpriseSettingsNavItems(runtime({ availability }))).toEqual([]);
    }
  });

  it("exposes only sign-in/overview before an authenticated Tenant exists", () => {
    expect(
      resolveEnterpriseSettingsNavItems(
        runtime({ authentication: "unauthenticated", hasSession: false, hasActiveTenant: false }),
      ).map((item) => item.id),
    ).toEqual(["organization-overview"]);
    expect(
      resolveEnterpriseSettingsNavItems(runtime({ hasActiveTenant: false })).map((item) => item.id),
    ).toEqual(["organization-overview"]);
  });

  it("filters authenticated destinations from server-derived presentation capabilities", () => {
    const items = resolveEnterpriseSettingsNavItems(
      runtime({
        capabilities: {
          ...NO_CAPABILITIES,
          canReadMembers: true,
          canReadIdentity: true,
          canReadCredentials: true,
          canReadQuota: true,
          canReadRetention: true,
          canReadAudit: true,
        },
      }),
    );
    expect(items.map((item) => item.id)).toEqual(
      ENTERPRISE_SETTINGS_NAV_ITEMS.map((item) => item.id),
    );
  });

  it("keeps support and data visible for their narrow read capabilities", () => {
    expect(
      resolveEnterpriseSettingsNavItems(
        runtime({ capabilities: { ...NO_CAPABILITIES, canReadOutbox: true } }),
      ).map((item) => item.id),
    ).toEqual(["organization-overview", "organization-support"]);
    expect(
      resolveEnterpriseSettingsNavItems(
        runtime({ capabilities: { ...NO_CAPABILITIES, canReadSchedulingPolicy: true } }),
      ).map((item) => item.id),
    ).toEqual(["organization-overview", "organization-data"]);
  });

  it("recognizes only registered enterprise destination ids", () => {
    expect(isEnterpriseSettingsSection("organization-identity")).toBe(true);
    expect(isEnterpriseSettingsSection("tenancy")).toBe(false);
    expect(isEnterpriseSettingsSection("advanced")).toBe(false);
  });
});
