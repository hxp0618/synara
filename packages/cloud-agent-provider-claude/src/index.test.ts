import { describe, expect, it } from "vitest";

import { createClaudeProvider } from "./index";

describe("createClaudeProvider", () => {
  it("publishes a host-neutral ABI descriptor", async () => {
    const provider = createClaudeProvider();
    const descriptor = await provider.describe();
    expect(provider.providerKind).toBe("claudeAgent");
    expect(descriptor.providerKind).toBe("claudeAgent");
    expect(descriptor.adapterVersion).toBe("claude-agent-sdk-v2");
    expect(descriptor.runtime.name).toBe("@anthropic-ai/claude-agent-sdk");
  });
});
