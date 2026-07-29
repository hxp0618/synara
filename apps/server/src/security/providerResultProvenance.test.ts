import { describe, expect, it } from "vitest";

import {
  PROVIDER_NATIVE_RESULT_PROVENANCE,
  providerHasModelPreconsumptionResultProvenance,
} from "./providerResultProvenance.ts";

describe("Provider result provenance capability", () => {
  it("admits only Providers with Host context for both result outcomes", () => {
    for (const provider of ["codex", "claudeAgent", "pi"] as const) {
      expect(providerHasModelPreconsumptionResultProvenance(provider)).toBe(true);
      expect(PROVIDER_NATIVE_RESULT_PROVENANCE[provider].successfulResult).not.toBe("policy-only");
      expect(PROVIDER_NATIVE_RESULT_PROVENANCE[provider].failedResult).not.toBe("policy-only");
    }

    for (const provider of [
      "cursor",
      "antigravity",
      "grok",
      "droid",
      "kilo",
      "opencode",
    ] as const) {
      expect(providerHasModelPreconsumptionResultProvenance(provider)).toBe(false);
    }
  });
});
