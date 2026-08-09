import { describe, expect, it } from "vitest";

import claudePackage from "../../cloud-agent-provider-claude/package.json";
import codexPackage from "../../cloud-agent-provider-codex/package.json";
import runtimePackage from "../../cloud-agent-runtime/package.json";
import distributionPackage from "../package.json";
import { CLOUD_AGENT_DISTRIBUTION_MANIFEST, createDefaultCloudAgentRuntime } from "./index";

describe("cloud-agent distribution", () => {
  it("registers only the pinned allowlisted providers", () => {
    const runtime = createDefaultCloudAgentRuntime();
    expect(runtime.providerKinds).toEqual(["claudeAgent", "codex"]);
    expect(CLOUD_AGENT_DISTRIBUTION_MANIFEST.protocol).toBe("2.3");
    expect(CLOUD_AGENT_DISTRIBUTION_MANIFEST.runtime).toEqual({
      package: "@synara/cloud-agent-runtime",
      version: "0.2.0",
    });
    expect(CLOUD_AGENT_DISTRIBUTION_MANIFEST.releaseDigest).toBeNull();
  });

  it("pins manifest versions to the package manifests and deeply freezes release metadata", () => {
    expect(CLOUD_AGENT_DISTRIBUTION_MANIFEST.distributionVersion).toBe(distributionPackage.version);
    expect(CLOUD_AGENT_DISTRIBUTION_MANIFEST.runtime.version).toBe(runtimePackage.version);
    expect(CLOUD_AGENT_DISTRIBUTION_MANIFEST.providers).toEqual([
      expect.objectContaining({ kind: "codex", version: codexPackage.version }),
      expect.objectContaining({ kind: "claudeAgent", version: claudePackage.version }),
    ]);
    expect(Object.isFrozen(CLOUD_AGENT_DISTRIBUTION_MANIFEST)).toBe(true);
    expect(Object.isFrozen(CLOUD_AGENT_DISTRIBUTION_MANIFEST.runtime)).toBe(true);
    expect(Object.isFrozen(CLOUD_AGENT_DISTRIBUTION_MANIFEST.providers)).toBe(true);
    expect(Object.isFrozen(CLOUD_AGENT_DISTRIBUTION_MANIFEST.providers[0])).toBe(true);
  });
});
