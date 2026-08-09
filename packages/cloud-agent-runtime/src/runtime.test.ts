import { describe, expect, it } from "vitest";

import {
  CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
  type CloudAgentProviderPluginV1,
} from "@synara/cloud-agent-provider-api";

import { createCloudAgentRuntime } from "./runtime";

function provider(providerKind: string): CloudAgentProviderPluginV1 {
  return {
    abiVersion: CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
    providerKind,
    describe: async () => ({
      abiVersion: CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
      providerKind,
      displayName: providerKind,
      adapterVersion: "test-v1",
      runtime: {
        kind: "local",
        name: "test",
        available: true,
        compatible: true,
        compatibleRange: { minimumInclusive: "0.0.0" },
      },
      capabilities: Object.create(null),
    }),
    createSession: async () => {
      throw new Error("not used");
    },
  };
}

describe("createCloudAgentRuntime", () => {
  it("registers providers explicitly and deterministically", () => {
    const runtime = createCloudAgentRuntime({ providers: [provider("codex"), provider("claude")] });
    expect(runtime.providerKinds).toEqual(["claude", "codex"]);
  });

  it("rejects duplicate providers", () => {
    expect(() =>
      createCloudAgentRuntime({ providers: [provider("codex"), provider("codex")] }),
    ).toThrow(/registered more than once/u);
  });
});
