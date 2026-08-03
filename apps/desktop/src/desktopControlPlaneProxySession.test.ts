import { describe, expect, it } from "vitest";

import {
  buildDesktopControlPlaneProxyHeaders,
  isDesktopControlPlaneProxyRequest,
  sanitizeDesktopControlPlaneProxyUrl,
} from "./desktopControlPlaneProxySession";

describe("Desktop Control Plane proxy session boundary", () => {
  const backend = "http://127.0.0.1:58180";

  it("matches only the exact loopback backend and /v1 route", () => {
    expect(isDesktopControlPlaneProxyRequest(`${backend}/v1/auth/session`, backend)).toBe(true);
    expect(isDesktopControlPlaneProxyRequest(`${backend}/api/auth/session`, backend)).toBe(false);
    expect(
      isDesktopControlPlaneProxyRequest("http://127.0.0.1:58181/v1/auth/session", backend),
    ).toBe(false);
    expect(
      isDesktopControlPlaneProxyRequest("https://control.example/v1/auth/session", backend),
    ).toBe(false);
  });

  it("strips only the local startup token before the request is proxied", () => {
    expect(
      sanitizeDesktopControlPlaneProxyUrl(
        `${backend}/v1/audit-logs?token=local-secret&limit=25`,
        backend,
      ),
    ).toBe(`${backend}/v1/audit-logs?limit=25`);
    expect(
      sanitizeDesktopControlPlaneProxyUrl(`${backend}/v1/audit-logs?limit=25`, backend),
    ).toBeNull();
  });

  it("removes browser cookies and replaces any renderer authorization", () => {
    expect(
      buildDesktopControlPlaneProxyHeaders(
        {
          Cookie: "local_session=secret",
          authorization: "Bearer renderer-controlled",
          Accept: "application/json",
        },
        "desktop-credential",
      ),
    ).toEqual({ Accept: "application/json", Authorization: "Bearer desktop-credential" });
  });
});
