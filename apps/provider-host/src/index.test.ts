import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

const root = resolve(import.meta.dirname, "../../..");
const releasePrefix =
  "https://github.com/hxp0618/cloud-agents/releases/download/cloud-agent-m1-rc.1/";

describe("external Cloud Agent Runtime boundary", () => {
  it("keeps provider-host as a one-import compatibility bin", () => {
    expect(readFileSync(resolve(import.meta.dirname, "index.ts"), "utf8").trim()).toBe(
      '#!/usr/bin/env node\nimport "@synara/cloud-agent-distribution/stdio";',
    );
  });

  it("does not retain editable public package or release-producer sources", () => {
    for (const path of [
      "packages/cloud-agent-protocol/package.json",
      "packages/cloud-agent-provider-api/package.json",
      "packages/cloud-agent-runtime/package.json",
      "packages/cloud-agent-provider-codex/package.json",
      "packages/cloud-agent-provider-claude/package.json",
      "packages/cloud-agent-testkit/package.json",
      "packages/cloud-agent-distribution/package.json",
      "scripts/cloud-agent-release-smoke.ts",
      "scripts/lib/cloud-agent-release.ts",
    ]) {
      expect(existsSync(resolve(root, path)), path).toBe(false);
    }
  });

  it("uses only the immutable GitHub RC asset dependency", () => {
    const manifest = JSON.parse(
      readFileSync(resolve(root, "apps/provider-host/package.json"), "utf8"),
    ) as { dependencies: Record<string, string> };
    expect(manifest.dependencies).toEqual({
      "@synara/cloud-agent-distribution": `${releasePrefix}synara-cloud-agent-distribution-0.1.0-rc.1.tgz`,
    });
  });

  it("packages the Worker from installed RC bits and embeds the candidate lock", () => {
    const dockerfile = readFileSync(resolve(root, "Dockerfile"), "utf8");
    expect(dockerfile).not.toMatch(/COPY packages\/cloud-agent-/u);
    expect(dockerfile).toContain("COPY --chown=0:0 --chmod=0444 cloud-agent-candidate.lock.json");
    expect(dockerfile).toContain("COPY deploy/worker/cloud-agent-candidate.mjs");
    expect(dockerfile).toContain("node_modules/@anthropic-ai/claude-agent-sdk/package.json");
    expect(dockerfile).toContain("--cloud-agent-candidate-lockfile");
  });
});
