import { describe, expect, it } from "vitest";

import { assertCloudAgentNode24, readCloudAgentBackendConfig } from "./config.ts";

describe("readCloudAgentBackendConfig", () => {
  it("defaults to the native Codex backend", () => {
    expect(readCloudAgentBackendConfig({})).toEqual({ backend: "native" });
  });

  it("selects Cloud Agent without widening the provider kind", () => {
    expect(readCloudAgentBackendConfig({ SYNARA_CODEX_BACKEND: "cloud-agent" })).toEqual({
      backend: "cloud-agent",
    });
  });

  it("fails closed on invalid selectors and digest-only overrides", () => {
    expect(() => readCloudAgentBackendConfig({ SYNARA_CODEX_BACKEND: "remote" })).toThrow(
      "SYNARA_CODEX_BACKEND",
    );
    expect(() =>
      readCloudAgentBackendConfig({ SYNARA_CLOUD_AGENT_RUNTIME_SHA256: "a".repeat(64) }),
    ).toThrow("requires SYNARA_CLOUD_AGENT_RUNTIME_PATH");
    expect(() =>
      readCloudAgentBackendConfig({ SYNARA_CLOUD_AGENT_RUNTIME_PATH: "/tmp/runtime.mjs" }),
    ).toThrow("requires SYNARA_CLOUD_AGENT_RUNTIME_SHA256");
  });

  it("requires the public runtime's Node range only after Cloud Agent is selected", () => {
    expect(() => assertCloudAgentNode24("24.13.1", undefined)).not.toThrow();
    expect(() => assertCloudAgentNode24("24.15.0", undefined)).not.toThrow();
    expect(() => assertCloudAgentNode24("24.13.0")).toThrow("requires Node.js >=24.13.1 <25");
    expect(() => assertCloudAgentNode24("22.22.0")).toThrow("requires Node.js >=24.13.1 <25");
    expect(() => assertCloudAgentNode24("25.1.0")).toThrow("requires Node.js >=24.13.1 <25");
    expect(() => assertCloudAgentNode24("24.13.1", "1.3.9")).toThrow(
      "requires Node.js >=24.13.1 <25",
    );
  });
});
