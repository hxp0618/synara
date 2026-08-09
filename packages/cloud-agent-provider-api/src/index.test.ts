import { describe, expect, it } from "vitest";

import { CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION } from "./index";

describe("Cloud Agent Provider Plugin ABI", () => {
  it("pins ABI major 1", () => {
    expect(CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION).toBe(1);
  });
});
