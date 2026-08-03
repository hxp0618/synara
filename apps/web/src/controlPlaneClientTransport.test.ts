// FILE: controlPlaneClientTransport.test.ts
// Purpose: Guard the host-only URL bridge used by the extracted Control Plane client package.

import {
  configureControlPlaneClientTransport,
  resolveControlPlaneHttpUrl,
} from "@synara/control-plane-client";
import { afterEach, describe, expect, it, vi } from "vitest";

import { configureWebControlPlaneClientTransport } from "./controlPlaneClientTransport";

afterEach(() => {
  configureControlPlaneClientTransport();
  vi.unstubAllGlobals();
});

describe("configureWebControlPlaneClientTransport", () => {
  it("bridges desktop custom-protocol requests through the authenticated WebSocket host", () => {
    vi.stubGlobal("window", {
      location: new URL("synara://desktop/settings"),
      desktopBridge: {
        getWsUrl: () => "ws://127.0.0.1:58090/?token=legacy-startup-token",
      },
    });

    configureWebControlPlaneClientTransport();

    expect(resolveControlPlaneHttpUrl("/v1/auth/session")).toBe(
      "http://127.0.0.1:58090/v1/auth/session?token=legacy-startup-token",
    );
  });
});
