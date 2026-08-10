import { createHash } from "node:crypto";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import {
  assertCloudAgentDependencyUrls,
  assertCloudAgentPublicSchema,
  assertCloudAgentRuntimeAsset,
  cloudAgentCandidateSha256,
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
    const repoRoot = join(import.meta.dirname, "../../..");
    const canonical = JSON.parse(
      await readFile(join(repoRoot, "cloud-agent-candidate.lock.json"), "utf8"),
    ) as Record<string, unknown>;
    const lockPath = join(directory, "cloud-agent-candidate.lock.json");
    try {
      await writeFile(lockPath, JSON.stringify(canonical));
      const lock = readCloudAgentCandidateLock(directory, { required: true })!;
      expect(cloudAgentCandidateSha256(lock.packages)).toBe(lock.candidateSha256);
      expect(cloudAgentCandidateSha256(lock.packages.toReversed())).toBe(lock.candidateSha256);
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

      const tamperedPackage = structuredClone(canonical) as {
        packages: Array<{ sha256: string }>;
      };
      tamperedPackage.packages[0]!.sha256 = `sha256:${"0".repeat(64)}`;
      await writeFile(lockPath, JSON.stringify(tamperedPackage));
      expect(() => readCloudAgentCandidateLock(directory, { required: true })).toThrow(
        "digest does not match",
      );

      const wrongSource = structuredClone(canonical) as { source: { commit: string } };
      wrongSource.source.commit = "0".repeat(40);
      await writeFile(lockPath, JSON.stringify(wrongSource));
      expect(() => readCloudAgentCandidateLock(directory, { required: true })).toThrow(
        "source or Release identity",
      );

      const wrongStandalone = structuredClone(canonical) as {
        standaloneRuntime: { sha256: string };
      };
      wrongStandalone.standaloneRuntime.sha256 = `sha256:${"0".repeat(64)}`;
      await writeFile(lockPath, JSON.stringify(wrongStandalone));
      expect(() => readCloudAgentCandidateLock(directory, { required: true })).toThrow(
        "standalone Runtime",
      );

      await writeFile(lockPath, JSON.stringify({ ...canonical, unexpected: true }));
      expect(() => readCloudAgentCandidateLock(directory, { required: true })).toThrow(
        "unexpected or missing fields",
      );
    } finally {
      await rm(directory, { recursive: true });
    }
  });
});
