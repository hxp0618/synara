import { describe, expect, it } from "vitest";

import { TenantPrivacyRequestsSettingsSection } from "./TenantPrivacyRequestsSettingsSection";

describe("TenantPrivacyRequestsSettingsSection module", () => {
  it("loads the DSAR workflow settings surface", () => {
    expect(TenantPrivacyRequestsSettingsSection).toBeTypeOf("function");
  });
});
