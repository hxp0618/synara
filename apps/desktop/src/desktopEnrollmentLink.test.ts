import { describe, expect, it } from "vitest";

import {
  DesktopEnrollmentLinkError,
  findDesktopEnrollmentLinkArgument,
  parseDesktopEnrollmentLink,
  parseDesktopEnrollmentOriginAllowlist,
} from "./desktopEnrollmentLink";

const HANDLE = "a".repeat(43);
const CONTROL_PLANE = "https://control.example.com/control-plane";

function link(
  parameters = new URLSearchParams({
    v: "1",
    control_plane: CONTROL_PLANE,
    enrollment: HANDLE,
  }),
) {
  return `synara://connect?${parameters.toString()}`;
}

describe("Desktop Enrollment link boundary", () => {
  const allowed = new Set([CONTROL_PLANE]);

  it("accepts only the frozen route, allowlisted base URL, and 256-bit handle", () => {
    expect(
      parseDesktopEnrollmentLink(link(), {
        scheme: "synara",
        allowedControlPlaneBaseUrls: allowed,
        allowLoopbackHttp: false,
      }),
    ).toEqual({ controlPlaneBaseUrl: CONTROL_PLANE, handle: HANDLE });
  });

  it("rejects unknown, duplicate, missing, cross-scheme, and disallowed-origin inputs", () => {
    const cases = [
      link(
        new URLSearchParams({
          v: "1",
          control_plane: CONTROL_PLANE,
          enrollment: HANDLE,
          token: "secret",
        }),
      ),
      `synara://connect?v=1&v=1&control_plane=${encodeURIComponent(CONTROL_PLANE)}&enrollment=${HANDLE}`,
      `synara://connect?v=1&control_plane=${encodeURIComponent(CONTROL_PLANE)}`,
      link().replace("synara://", "synara-canary://"),
      link().replace(encodeURIComponent(CONTROL_PLANE), encodeURIComponent("https://evil.example")),
      `synara://other?v=1&control_plane=${encodeURIComponent(CONTROL_PLANE)}&enrollment=${HANDLE}`,
    ];
    for (const candidate of cases) {
      expect(() =>
        parseDesktopEnrollmentLink(candidate, {
          scheme: "synara",
          allowedControlPlaneBaseUrls: allowed,
          allowLoopbackHttp: false,
        }),
      ).toThrow(DesktopEnrollmentLinkError);
    }
  });

  it("never includes the raw handle or URL in parse errors", () => {
    try {
      parseDesktopEnrollmentLink(link().replace(HANDLE, "bad"), {
        scheme: "synara",
        allowedControlPlaneBaseUrls: allowed,
        allowLoopbackHttp: false,
      });
      throw new Error("expected parse failure");
    } catch (error) {
      expect(String(error)).not.toContain(HANDLE);
      expect(String(error)).not.toContain(CONTROL_PLANE);
    }
  });

  it("builds a normalized explicit allowlist and isolates launch arguments", () => {
    expect([
      ...parseDesktopEnrollmentOriginAllowlist({
        raw: "https://control.example.com/control-plane/, http://127.0.0.1:58180, file:///tmp/no",
        allowLoopbackHttp: true,
      }),
    ]).toEqual([CONTROL_PLANE, "http://127.0.0.1:58180"]);
    expect(findDesktopEnrollmentLinkArgument(["Synara", "--flag", link()], "synara")).toBe(link());
    expect(findDesktopEnrollmentLinkArgument(["Synara", link()], "synara-canary")).toBeNull();
  });
});
