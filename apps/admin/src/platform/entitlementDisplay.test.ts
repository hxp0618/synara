import { describe, expect, it } from "vitest";

import { entitlementProfileLabel, tenantLifecycleLabel } from "./entitlementDisplay";

describe("internal entitlement display", () => {
  it("renders internal entitlement profiles without subscription language", () => {
    expect(entitlementProfileLabel("enterprise")).toBe("Enterprise profile");
    expect(entitlementProfileLabel("standard")).toBe("Standard profile");
    expect(entitlementProfileLabel("restricted")).toBe("restricted profile");
    expect(tenantLifecycleLabel("evaluation")).toBe("evaluation");
    expect(tenantLifecycleLabel("active")).toBe("active");
  });
});
