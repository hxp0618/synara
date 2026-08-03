import { describe, expect, it } from "vitest";

import { TenantLegalHoldsSettingsSection } from "./TenantLegalHoldsSettingsSection";

describe("TenantLegalHoldsSettingsSection module", () => {
  it("loads the governed Legal Hold settings surface", () => {
    expect(TenantLegalHoldsSettingsSection).toBeTypeOf("function");
  });
});
