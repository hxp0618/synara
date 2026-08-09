import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

import {
  canonicalCloudAgentCandidateDigest,
  validateCloudAgentCandidateLock,
} from "../deploy/worker/cloud-agent-candidate.mjs";
import type { CloudAgentPackageName } from "../deploy/worker/cloud-agent-candidate.mjs";

const lockText = readFileSync(
  resolve(import.meta.dirname, "../cloud-agent-candidate.lock.json"),
  "utf8",
);

type MutableArtifact = { version: string; url: string; sha256: `sha256:${string}` };
type MutableCandidateLock = {
  release: {
    repository: string;
    tag: string;
    sourceCommit: string;
    candidateDigest: `sha256:${string}`;
  };
  standaloneRuntime: { url: string; sha256: `sha256:${string}` };
  packages: Record<CloudAgentPackageName, MutableArtifact>;
};

function parsedLock(): MutableCandidateLock {
  return JSON.parse(lockText) as MutableCandidateLock;
}

describe("Cloud Agent candidate lock", () => {
  it("accepts the exact immutable RC and reproduces the producer digest", () => {
    const candidate = validateCloudAgentCandidateLock(lockText);
    expect(candidate.candidateDigest).toBe(
      "sha256:b9931233d46aeaf1392197095483c2e3409f628a47b2ba92c8e57bb38b444676",
    );
    expect(canonicalCloudAgentCandidateDigest(candidate.packages)).toBe(candidate.candidateDigest);
    expect(candidate.packages).toHaveLength(7);
  });

  it.each([
    [
      "repository",
      (lock: MutableCandidateLock) => (lock.release.repository = "example/cloud-agents"),
    ],
    ["tag", (lock: MutableCandidateLock) => (lock.release.tag = "mutable")],
    ["source", (lock: MutableCandidateLock) => (lock.release.sourceCommit = "a".repeat(40))],
    ["standalone URL", (lock: MutableCandidateLock) => (lock.standaloneRuntime.url += ".wrong")],
    [
      "standalone SHA",
      (lock: MutableCandidateLock) => (lock.standaloneRuntime.sha256 = `sha256:${"a".repeat(64)}`),
    ],
    [
      "package version",
      (lock: MutableCandidateLock) =>
        (lock.packages["@synara/cloud-agent-runtime"].version = "0.2.0-rc.2"),
    ],
    [
      "package URL",
      (lock: MutableCandidateLock) =>
        (lock.packages["@synara/cloud-agent-protocol"].url = "https://example.test/protocol.tgz"),
    ],
    [
      "package SHA",
      (lock: MutableCandidateLock) =>
        (lock.packages["@synara/cloud-agent-protocol"].sha256 = `sha256:${"a".repeat(64)}`),
    ],
  ])("rejects a mismatched %s", (_label, mutate) => {
    const lock = parsedLock();
    mutate(lock);
    expect(() => validateCloudAgentCandidateLock(lock)).toThrow();
  });

  it("rejects a self-consistent digest over a forged tuple", () => {
    const lock = parsedLock();
    lock.packages["@synara/cloud-agent-protocol"].sha256 = `sha256:${"a".repeat(64)}`;
    lock.release.candidateDigest = canonicalCloudAgentCandidateDigest(
      (Object.entries(lock.packages) as Array<[CloudAgentPackageName, MutableArtifact]>).map(
        ([name, artifact]) => ({ name, version: artifact.version, sha256: artifact.sha256 }),
      ),
    );
    expect(() => validateCloudAgentCandidateLock(lock)).toThrow(/tuple does not match/);
  });
});
