import { describe, expect, it } from "vitest";

import { resolveControlPlaneProxyTarget } from "../controlPlaneProxyTarget.mjs";

describe("Admin Control Plane proxy target", () => {
  it("preserves HTTPS and a fixed deployment base path", () => {
    expect(
      resolveControlPlaneProxyTarget(
        new URL("https://gateway.example/control-plane/"),
        "/v1/platform/tenants?limit=25",
      ).toString(),
    ).toBe("https://gateway.example/control-plane/v1/platform/tenants?limit=25");
  });

  it("keeps root HTTP deployments compatible for local development", () => {
    expect(
      resolveControlPlaneProxyTarget(
        new URL("http://127.0.0.1:3780"),
        "/v1/auth/session",
      ).toString(),
    ).toBe("http://127.0.0.1:3780/v1/auth/session");
  });
});
