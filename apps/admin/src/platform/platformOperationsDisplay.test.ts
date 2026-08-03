import { describe, expect, it } from "vitest";

import { platformTenant } from "../testFixtures";
import {
  formatBytes,
  formatOldestQueueAge,
  platformTenantOperationsLines,
} from "./platformOperationsDisplay";

describe("Platform operations display", () => {
  it("formats all operational resource groups with queue age", () => {
    expect(platformTenantOperationsLines(platformTenant, "2026-07-30T10:30:00Z")).toEqual([
      "12 members · 2 Organizations · 18 Sessions",
      "3 Targets · 6 Workers · 1 offline",
      "4 active Executions · 2 queued (1h 15m oldest) · 1 failed in 24h",
      "24 Artifacts · 2 pending · 1.5 MiB",
      "3 active Credentials · 1 unavailable · 1 active / 1 disabled Identity Connections",
    ]);
  });

  it("keeps empty or invalid measurements predictable", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatOldestQueueAge(null, "2026-07-30T10:30:00Z")).toBe("");
    expect(formatOldestQueueAge("invalid", "2026-07-30T10:30:00Z")).toBe("");
  });
});
