import { describe, expect, it } from "vitest";

import {
  buildControlPlaneProxyRequestHeaders,
  buildControlPlaneProxyResponseHeaders,
  resolveControlPlaneTarget,
  shouldStreamControlPlaneResponse,
} from "./controlPlaneProxy";

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

  it("forwards cookies while replacing untrusted forwarded headers", () => {
    const headers = buildControlPlaneProxyRequestHeaders({
      headers: {
        cookie: "synara_login=session-token",
        connection: "keep-alive",
        "x-forwarded-for": "203.0.113.10",
        "x-forwarded-host": "spoofed.example",
        "x-forwarded-proto": "http",
      },
      requestUrl: new URL("https://synara.example/v1/auth/session"),
      remoteAddress: "127.0.0.1",
    });

    expect(headers.get("cookie")).toBe("synara_login=session-token");
    expect(headers.get("connection")).toBeNull();
    expect(headers.get("x-forwarded-for")).toBe("127.0.0.1");
    expect(headers.get("x-forwarded-host")).toBe("synara.example");
    expect(headers.get("x-forwarded-proto")).toBe("https");
  });

  it("does not retain a spoofed forwarded address when the socket address is unavailable", () => {
    const headers = buildControlPlaneProxyRequestHeaders({
      headers: { "x-forwarded-for": "203.0.113.10" },
      requestUrl: new URL("https://synara.example/v1/platform/profile"),
    });

    expect(headers.get("x-forwarded-for")).toBeNull();
  });

  it("preserves multiple login cookies from the Control Plane", () => {
    const upstreamHeaders = new Headers({ "Content-Type": "application/json" });
    upstreamHeaders.append("Set-Cookie", "session=one; Path=/; HttpOnly");
    upstreamHeaders.append("Set-Cookie", "csrf=two; Path=/; SameSite=Lax");
    const headers = buildControlPlaneProxyResponseHeaders(
      new Response("{}", { headers: upstreamHeaders }),
    );

    expect(headers["set-cookie"]).toEqual([
      "session=one; Path=/; HttpOnly",
      "csrf=two; Path=/; SameSite=Lax",
    ]);
    expect(headers["content-type"]).toBe("application/json");
  });
});
