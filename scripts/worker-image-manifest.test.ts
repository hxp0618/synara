import { describe, expect, it } from "vitest";

// @ts-expect-error -- the Worker image helper is an executable ESM script without a published
// declaration file; keep the test-side contract explicit here.
import * as workerImageManifestModule from "../deploy/worker/worker-image-manifest.mjs";

type WorkerImageBaseImage = {
  name: string;
  reference: string;
};

type WorkerImageProviderRuntime = {
  provider: string;
  kind: string;
  package: string;
  version: string;
};

type WorkerImageLockfile = {
  name: string;
  path: string;
  sha256: string;
};

type WorkerImageSbom = {
  name: string;
  format: string;
  path: string;
  sha256: string;
};

type WorkerImageArtifacts = {
  manifest: {
    schemaVersion: number;
    source: { version: string; gitSha: string };
    platform: { os: string; architecture: string };
    baseImages: WorkerImageBaseImage[];
    lockfiles: WorkerImageLockfile[];
    providerRuntimes: WorkerImageProviderRuntime[];
    sboms: WorkerImageSbom[];
  };
  manifestJSON: string;
  providerToolsSBOM: string;
};

const { buildWorkerImageArtifacts, sha256Hex } = workerImageManifestModule as {
  buildWorkerImageArtifacts(input: {
    version: string;
    gitSHA: string;
    sourceDateEpoch: string;
    architecture: string;
    baseImages: string[];
    providerToolsLockfile: string;
    providerHostLockfile: string;
    providerHostPackageJSON: string;
    workerAPKLockfile: string;
    rawProviderToolsSBOM: string;
  }): WorkerImageArtifacts;
  sha256Hex(value: string): string;
};

const gitSHA = "c0efe20098f71ce23ae0099769313ea0fd2d7bf0";
const baseImages = [
  "worker-runtime=node:24-alpine@sha256:" + "a".repeat(64),
  "provider-host-build=oven/bun:1.3.14@sha256:" + "b".repeat(64),
  "agentd-build=golang:1.26-bookworm@sha256:" + "c".repeat(64),
];
const providerToolsLockfile = `${JSON.stringify(
  {
    lockfileVersion: 3,
    packages: {
      "": {
        dependencies: {
          "@anthropic-ai/claude-code": "2.1.197",
          "@openai/codex": "0.144.1",
        },
      },
      "node_modules/@anthropic-ai/claude-code": { version: "2.1.197" },
      "node_modules/@openai/codex": {
        version: "0.144.1",
        optionalDependencies: {
          "@openai/codex-linux-arm64": "npm:@openai/codex@0.144.1-linux-arm64",
        },
      },
      "node_modules/@openai/codex-linux-arm64": {
        name: "@openai/codex",
        version: "0.144.1-linux-arm64",
        optional: true,
        os: ["linux"],
        cpu: ["arm64"],
      },
    },
  },
  null,
  2,
)}\n`;
const providerHostPackageJSON = `${JSON.stringify({
  dependencies: { "@anthropic-ai/claude-agent-sdk": "0.3.207" },
})}\n`;
const workerAPKLockfile = `
bash=5.3.9-r1
ca-certificates=20260611-r0
git=2.54.0-r0
jq=1.8.1-r0
libcurl=8.21.0-r0
openssh-client-default=10.3_p1-r0
ripgrep=15.1.0-r0
`;

function rawSBOM(created: string, namespace: string) {
  return JSON.stringify({
    spdxVersion: "SPDX-2.3",
    dataLicense: "CC0-1.0",
    SPDXID: "SPDXRef-DOCUMENT",
    name: "random-name",
    documentNamespace: namespace,
    creationInfo: { created, creators: ["Tool: npm/cli-random"] },
    documentDescribes: ["SPDXRef-openai", "SPDXRef-anthropic"],
    packages: [
      { SPDXID: "SPDXRef-openai", name: "@openai/codex", versionInfo: "0.144.1" },
      {
        SPDXID: "SPDXRef-openai-platform",
        name: "@openai/codex",
        versionInfo: "0.144.1-linux-arm64",
      },
      { SPDXID: "SPDXRef-anthropic", name: "@anthropic-ai/claude-code", versionInfo: "2.1.197" },
    ],
    relationships: [],
  });
}

function build(overrides: Record<string, unknown> = {}) {
  return buildWorkerImageArtifacts({
    version: "0.5.3",
    gitSHA,
    sourceDateEpoch: "1784020000",
    architecture: "arm64",
    baseImages,
    providerToolsLockfile,
    providerHostLockfile: "locked bun graph\n",
    providerHostPackageJSON,
    workerAPKLockfile,
    rawProviderToolsSBOM: rawSBOM("2026-01-01T00:00:00.000Z", "urn:uuid:random-a"),
    ...overrides,
  });
}

describe("Worker image manifest", () => {
  it("normalizes the SPDX document and records every immutable build input", () => {
    const first = build();
    const second = build({
      rawProviderToolsSBOM: rawSBOM("2030-01-01T00:00:00.000Z", "urn:uuid:random-b"),
    });

    expect(first.providerToolsSBOM).toBe(second.providerToolsSBOM);
    expect(first.manifest).toEqual(second.manifest);
    expect(first.manifest.source).toEqual({ version: "0.5.3", gitSha: gitSHA });
    expect(first.manifest.platform).toEqual({ os: "linux", architecture: "arm64" });
    expect(first.manifest.baseImages.map((entry) => entry.name)).toEqual([
      "agentd-build",
      "provider-host-build",
      "worker-runtime",
    ]);
    expect(first.manifest.providerRuntimes).toEqual([
      {
        provider: "claudeAgent",
        kind: "cli",
        package: "@anthropic-ai/claude-code",
        version: "2.1.197",
      },
      {
        provider: "claudeAgent",
        kind: "sdk",
        package: "@anthropic-ai/claude-agent-sdk",
        version: "0.3.207",
      },
      { provider: "codex", kind: "cli", package: "@openai/codex", version: "0.144.1" },
    ]);
    expect(first.manifest.lockfiles).toHaveLength(3);
    expect(first.manifest.sboms[0]?.sha256).toBe(sha256Hex(first.providerToolsSBOM));
    const normalizedSBOM = JSON.parse(first.providerToolsSBOM);
    expect(normalizedSBOM.creationInfo).toEqual({
      created: new Date(1784020000 * 1000).toISOString(),
      creators: ["Tool: synara-worker-image-manifest/1"],
    });
    expect(normalizedSBOM.documentNamespace).toContain(sha256Hex(providerToolsLockfile));
  });

  it("rejects mutable base images and incomplete dependency locks", () => {
    expect(() =>
      build({
        baseImages: [...baseImages.slice(0, 2), "agentd-build=golang:1.26-bookworm"],
      }),
    ).toThrow(/immutable sha256/);

    expect(() => build({ workerAPKLockfile: "bash=5.3.9-r1\n" })).toThrow(
      /missing ca-certificates/,
    );
    expect(() =>
      build({
        rawProviderToolsSBOM: rawSBOM("2026-01-01T00:00:00.000Z", "urn:uuid:missing").replace(
          '"0.144.1"',
          '"0.144.0"',
        ),
      }),
    ).toThrow(/does not describe @openai\/codex@0.144.1/);

    const withoutCodexPlatform = JSON.parse(
      rawSBOM("2026-01-01T00:00:00.000Z", "urn:uuid:missing-platform"),
    );
    withoutCodexPlatform.packages = withoutCodexPlatform.packages.filter(
      (entry: { versionInfo?: string }) => entry.versionInfo !== "0.144.1-linux-arm64",
    );
    expect(() => build({ rawProviderToolsSBOM: JSON.stringify(withoutCodexPlatform) })).toThrow(
      /does not describe @openai\/codex@0.144.1-linux-arm64/,
    );
  });
});
