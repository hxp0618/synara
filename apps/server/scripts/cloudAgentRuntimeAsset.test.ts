import { createHash } from "node:crypto";
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import {
  assertCloudAgentDependencyUrls,
  assertCloudAgentPublicSchema,
  assertCloudAgentRuntimeAsset,
  readCloudAgentCandidateLock,
} from "./cloudAgentRuntimeAsset.ts";

describe("Cloud Agent Runtime package asset", () => {
  it("uses the stable public Protocol v2 schema", () => {
    expect(() => assertCloudAgentPublicSchema()).not.toThrow();
  });

  it("requires non-empty executable bytes and an exact candidate digest", async () => {
    const directory = await mkdtemp(join(tmpdir(), "synara-cloud-agent-asset-"));
    try {
      const assetPath = join(directory, "runtime.mjs");
      await writeFile(assetPath, "#!/usr/bin/env node\n");
      await chmod(assetPath, 0o755);
      const expectedSha256 = `sha256:${createHash("sha256")
        .update("#!/usr/bin/env node\n")
        .digest("hex")}`;
      expect(() => assertCloudAgentRuntimeAsset({ assetPath, expectedSha256 })).not.toThrow();
      expect(() =>
        assertCloudAgentRuntimeAsset({
          assetPath,
          expectedSha256: `sha256:${"0".repeat(64)}`,
        }),
      ).toThrow("digest mismatch");
    } finally {
      await rm(directory, { recursive: true });
    }
  });

  it("requires a complete seven-package immutable Release lock", async () => {
    const directory = await mkdtemp(join(tmpdir(), "synara-cloud-agent-lock-"));
    const baseUrl = "https://github.com/hxp0618/cloud-agents/releases/download/cloud-agent-m1-rc.1";
    const packageVersions = {
      "@synara/cloud-agent-distribution": "0.1.0-rc.1",
      "@synara/cloud-agent-protocol": "0.1.0-rc.1",
      "@synara/cloud-agent-provider-api": "0.1.0-rc.1",
      "@synara/cloud-agent-provider-claude": "0.1.0-rc.1",
      "@synara/cloud-agent-provider-codex": "0.1.0-rc.1",
      "@synara/cloud-agent-runtime": "0.2.0-rc.1",
      "@synara/cloud-agent-testkit": "0.1.0-rc.1",
    } as const;
    const packages = Object.entries(packageVersions).map(([name, version], index) => {
      const filename = `${name.slice(1).replace("/", "-")}-${version}.tgz`;
      return {
        name,
        version,
        filename,
        url: `${baseUrl}/${filename}`,
        sha256: `sha256:${String(index).padStart(64, "0")}`,
      };
    });
    try {
      await writeFile(
        join(directory, "cloud-agent-candidate.lock.json"),
        JSON.stringify({
          schemaVersion: 1,
          kind: "synara-cloud-agent-release-candidate-lock",
          source: {
            repository: "https://github.com/hxp0618/cloud-agents",
            commit: "a".repeat(40),
          },
          release: { tag: "cloud-agent-m1-rc.1", baseUrl },
          candidateSha256: `sha256:${"a".repeat(64)}`,
          standaloneRuntime: {
            filename: "cloud-agent-runtime-standalone.mjs",
            url: `${baseUrl}/cloud-agent-runtime-standalone.mjs`,
            sha256: `sha256:${"b".repeat(64)}`,
          },
          packages,
        }),
      );
      const lock = readCloudAgentCandidateLock(directory, { required: true })!;
      const dependencies = Object.fromEntries(
        lock.packages
          .filter((artifact) => artifact.name !== "@synara/cloud-agent-testkit")
          .map((artifact) => [artifact.name, artifact.url]),
      );
      expect(() => assertCloudAgentDependencyUrls(dependencies, lock)).not.toThrow();
      expect(() =>
        assertCloudAgentDependencyUrls(
          { ...dependencies, "@synara/cloud-agent-runtime": "0.2.0-rc.1" },
          lock,
        ),
      ).toThrow("does not match");
    } finally {
      await rm(directory, { recursive: true });
    }
  });
});
