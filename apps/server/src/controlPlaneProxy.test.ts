import { describe, expect, it } from "vitest";

import { resolveControlPlaneTarget, shouldStreamControlPlaneResponse } from "./controlPlaneProxy";

describe("resolveControlPlaneTarget", () => {
  it("keeps the public path and query while replacing the internal origin", () => {
    expect(
      resolveControlPlaneTarget(
        new URL("http://control-plane:3780"),
        new URL("https://synara.example/v1/tenants?limit=25"),
      ).toString(),
    ).toBe("http://control-plane:3780/v1/tenants?limit=25");
  });

  it("preserves SCIM paths for enterprise directory providers", () => {
    expect(
      resolveControlPlaneTarget(
        new URL("http://control-plane:3780"),
        new URL("https://synara.example/scim/v2/Users?count=100"),
      ).toString(),
    ).toBe("http://control-plane:3780/scim/v2/Users?count=100");
  });

  it("streams event-stream responses without buffering them", () => {
    const response = new Response(new ReadableStream(), {
      headers: { "Content-Type": "text/event-stream; charset=utf-8" },
    });
    expect(shouldStreamControlPlaneResponse(response)).toBe(true);
    expect(shouldStreamControlPlaneResponse(new Response("{}"))).toBe(false);
  });

  it("streams audit and Artifact attachments without buffering them", () => {
    const response = new Response(new ReadableStream(), {
      headers: {
        "Content-Type": "text/csv; charset=utf-8",
        "Content-Disposition": 'attachment; filename="audit.csv"',
      },
    });
    expect(shouldStreamControlPlaneResponse(response)).toBe(true);
  });
});
