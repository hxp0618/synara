import { describe, expect, it } from "vitest";

import { createCodexProvider } from "./index";

describe("createCodexProvider", () => {
  it("publishes a host-neutral ABI descriptor", async () => {
    const provider = createCodexProvider();
    const descriptor = await provider.describe();
    expect(provider.providerKind).toBe("codex");
    expect(descriptor.providerKind).toBe("codex");
    expect(descriptor.adapterVersion).toBe("codex-app-server-v2");
    expect(descriptor.runtime.name).toBe("codex");
  });
});
