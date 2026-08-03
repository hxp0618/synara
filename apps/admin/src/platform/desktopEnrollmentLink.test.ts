import { describe, expect, it } from "vitest";

import { buildDesktopEnrollmentLink } from "./desktopEnrollmentLink";

const HANDLE = "a".repeat(43);

describe("buildDesktopEnrollmentLink", () => {
  it("encodes only the frozen v1 origin and opaque handle", () => {
    const link = buildDesktopEnrollmentLink({
      controlPlaneOrigin: "https://control.example.com/control-plane",
      handle: HANDLE,
    });

    expect(link).toBe(
      `synara://connect?v=1&control_plane=https%3A%2F%2Fcontrol.example.com%2Fcontrol-plane&enrollment=${HANDLE}`,
    );
    expect(link).not.toContain("token=");
    expect(link).not.toContain("session=");
  });

  it("allows explicit Canary and loopback development links", () => {
    expect(
      buildDesktopEnrollmentLink({
        controlPlaneOrigin: "http://127.0.0.1:58180",
        handle: HANDLE,
        scheme: "synara-canary",
      }),
    ).toContain("synara-canary://connect?");
  });

  it("rejects non-TLS remote origins, credentials, fragments, and malformed handles", () => {
    for (const controlPlaneOrigin of [
      "http://control.example.com",
      "https://user@control.example.com",
      "https://control.example.com/#fragment",
      "file:///tmp/control-plane",
    ]) {
      expect(() => buildDesktopEnrollmentLink({ controlPlaneOrigin, handle: HANDLE })).toThrow();
    }
    expect(() =>
      buildDesktopEnrollmentLink({
        controlPlaneOrigin: "https://control.example.com",
        handle: "not-a-handle",
      }),
    ).toThrow();
  });
});
