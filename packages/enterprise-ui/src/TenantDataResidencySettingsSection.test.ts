import { describe, expect, it } from "vitest";

import {
  buildResidencyStatement,
  TenantDataResidencySettingsSection,
} from "./TenantDataResidencySettingsSection";

describe("TenantDataResidencySettingsSection module", () => {
  it("loads the versioned execution Region settings surface", () => {
    expect(TenantDataResidencySettingsSection).toBeTypeOf("function");
  });

  it("builds a host-downloadable statement without touching browser globals", () => {
    const statement = buildResidencyStatement({
      tenantId: "tenant-1",
      tenantName: "Tenant One",
      homeRegion: "cn-east-1",
      policy: {
        scope: {
          scopeKind: "tenant",
          scopeId: "tenant-1",
          version: 4,
          digest: "sha256:policy",
          document: {
            denyAll: false,
            target: { mode: "any", values: [] },
            region: { mode: "allow", values: ["cn-east-1"] },
            cluster: { mode: "any", values: [] },
            provider: { mode: "any", values: [] },
            capacityClass: { mode: "any", values: [] },
          },
        },
        effective: {
          denyAll: false,
          target: { mode: "any", values: [] },
          region: { mode: "allow", values: ["cn-east-1"] },
          cluster: { mode: "any", values: [] },
          provider: { mode: "any", values: [] },
          capacityClass: { mode: "any", values: [] },
        },
      },
    });

    expect(statement.execution).toMatchObject({
      status: "enforced",
      allowedRegions: ["cn-east-1"],
      policyVersion: 4,
    });
  });
});
