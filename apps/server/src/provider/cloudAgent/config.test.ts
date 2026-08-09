import { describe, expect, it } from "vitest";

import { readCloudAgentBackendConfig } from "./config.ts";

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
  });
});
