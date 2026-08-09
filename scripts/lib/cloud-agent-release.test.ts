import { createHash } from "node:crypto";
import { describe, expect, it } from "vitest";

import {
  assertSameCloudAgentBits,
  cloudAgentStableImportSpecifiers,
  cloudAgentTarballClosure,
  cloudAgentCandidateDigest,
  CLOUD_AGENT_PUBLIC_PACKAGES,
  parseCloudAgentReleaseSmokeOptions,
  type PackedCloudAgentPackage,
  validatePackedCloudAgentManifest,
  validatePackedCloudAgentSet,
} from "./cloud-agent-release";

function packed(
  name: (typeof CLOUD_AGENT_PUBLIC_PACKAGES)[number],
  version = name === "@synara/cloud-agent-runtime" ? "0.2.0" : "0.1.0",
): PackedCloudAgentPackage {
  return {
    name,
    version,
    filename: `${name.split("/").at(-1)}-${version}.tgz`,
    sha256: `sha256:${createHash("sha256").update(name).digest("hex")}`,
  };
}

type TestManifest = {
  name: (typeof CLOUD_AGENT_PUBLIC_PACKAGES)[number];
  version: string;
  dependencies: Record<string, string>;
};

function validManifests(): TestManifest[] {
  const versions = Object.fromEntries(
    CLOUD_AGENT_PUBLIC_PACKAGES.map((name) => [
      name,
      name === "@synara/cloud-agent-runtime" ? "0.2.0" : "0.1.0",
    ]),
  ) as Record<(typeof CLOUD_AGENT_PUBLIC_PACKAGES)[number], string>;
  const dependencies = {
    "@synara/cloud-agent-protocol": {},
    "@synara/cloud-agent-provider-api": {
      "@synara/cloud-agent-protocol": versions["@synara/cloud-agent-protocol"],
    },
    "@synara/cloud-agent-runtime": {
      "@synara/cloud-agent-protocol": versions["@synara/cloud-agent-protocol"],
      "@synara/cloud-agent-provider-api": versions["@synara/cloud-agent-provider-api"],
    },
    "@synara/cloud-agent-provider-codex": {
      "@synara/cloud-agent-provider-api": versions["@synara/cloud-agent-provider-api"],
    },
    "@synara/cloud-agent-provider-claude": {
      "@anthropic-ai/claude-agent-sdk": "0.3.207",
      "@synara/cloud-agent-provider-api": versions["@synara/cloud-agent-provider-api"],
    },
    "@synara/cloud-agent-testkit": {
      "@synara/cloud-agent-protocol": versions["@synara/cloud-agent-protocol"],
      "@synara/cloud-agent-provider-api": versions["@synara/cloud-agent-provider-api"],
    },
    "@synara/cloud-agent-distribution": {
      "@synara/cloud-agent-protocol": versions["@synara/cloud-agent-protocol"],
      "@synara/cloud-agent-provider-api": versions["@synara/cloud-agent-provider-api"],
      "@synara/cloud-agent-runtime": versions["@synara/cloud-agent-runtime"],
      "@synara/cloud-agent-provider-codex": versions["@synara/cloud-agent-provider-codex"],
      "@synara/cloud-agent-provider-claude": versions["@synara/cloud-agent-provider-claude"],
    },
  } satisfies Record<(typeof CLOUD_AGENT_PUBLIC_PACKAGES)[number], Record<string, string>>;
  return CLOUD_AGENT_PUBLIC_PACKAGES.map((name) => ({
    name,
    version: versions[name],
    dependencies: dependencies[name],
  }));
}

function replaceDependencies(
  manifests: ReadonlyArray<TestManifest>,
  name: TestManifest["name"],
  dependencies: Record<string, string>,
): TestManifest[] {
  return manifests.map((manifest) =>
    manifest.name === name
      ? { name: manifest.name, version: manifest.version, dependencies }
      : manifest,
  );
}

describe("Cloud Agent packed release validation", () => {
  it("isolates every package import to its exact transitive tarball closure", () => {
    const expected = {
      "@synara/cloud-agent-protocol": ["@synara/cloud-agent-protocol"],
      "@synara/cloud-agent-provider-api": [
        "@synara/cloud-agent-protocol",
        "@synara/cloud-agent-provider-api",
      ],
      "@synara/cloud-agent-runtime": [
        "@synara/cloud-agent-protocol",
        "@synara/cloud-agent-provider-api",
        "@synara/cloud-agent-runtime",
      ],
      "@synara/cloud-agent-provider-codex": [
        "@synara/cloud-agent-protocol",
        "@synara/cloud-agent-provider-api",
        "@synara/cloud-agent-provider-codex",
      ],
      "@synara/cloud-agent-provider-claude": [
        "@synara/cloud-agent-protocol",
        "@synara/cloud-agent-provider-api",
        "@synara/cloud-agent-provider-claude",
      ],
      "@synara/cloud-agent-testkit": [
        "@synara/cloud-agent-protocol",
        "@synara/cloud-agent-provider-api",
        "@synara/cloud-agent-testkit",
      ],
      "@synara/cloud-agent-distribution": [
        "@synara/cloud-agent-protocol",
        "@synara/cloud-agent-provider-api",
        "@synara/cloud-agent-runtime",
        "@synara/cloud-agent-provider-codex",
        "@synara/cloud-agent-provider-claude",
        "@synara/cloud-agent-distribution",
      ],
    } satisfies Record<
      (typeof CLOUD_AGENT_PUBLIC_PACKAGES)[number],
      ReadonlyArray<(typeof CLOUD_AGENT_PUBLIC_PACKAGES)[number]>
    >;
    for (const target of CLOUD_AGENT_PUBLIC_PACKAGES) {
      expect(cloudAgentTarballClosure(target)).toEqual(expected[target]);
    }
  });

  it("imports stable public subpaths in the isolated target environment", () => {
    expect(cloudAgentStableImportSpecifiers("@synara/cloud-agent-provider-api")).toEqual([
      "@synara/cloud-agent-provider-api",
      "@synara/cloud-agent-provider-api/internal",
    ]);
    expect(cloudAgentStableImportSpecifiers("@synara/cloud-agent-runtime")).toEqual([
      "@synara/cloud-agent-runtime",
      "@synara/cloud-agent-runtime/node",
    ]);
    expect(cloudAgentStableImportSpecifiers("@synara/cloud-agent-distribution")).toEqual([
      "@synara/cloud-agent-distribution",
      "@synara/cloud-agent-distribution/schemas",
      "@synara/cloud-agent-distribution/schemas/cloud-agent-envelope-v2",
    ]);
  });

  it("rejects local protocols and unpublished private dependencies", () => {
    expect(() =>
      validatePackedCloudAgentManifest({
        name: "@synara/cloud-agent-runtime",
        version: "0.2.0",
        dependencies: { "@synara/cloud-agent-protocol": "workspace:*" },
      }),
    ).toThrow(/local protocol/);
    expect(() =>
      validatePackedCloudAgentManifest({
        name: "@synara/cloud-agent-runtime",
        version: "0.2.0",
        dependencies: { "@synara/contracts": "0.0.0" },
      }),
    ).toThrow(/unpublished private package/);
    expect(() =>
      validatePackedCloudAgentManifest({
        name: "@synara/cloud-agent-runtime",
        version: "0.2.0",
        exports: { "./legacy-provider-host": "./dist/legacyProviderHost.mjs" },
      }),
    ).toThrow(/legacy Provider facade/);
  });

  it("requires all seven tarballs and exact cross-package pins", () => {
    const manifests = validManifests();
    expect(() => validatePackedCloudAgentSet(manifests)).not.toThrow();
    expect(() =>
      validatePackedCloudAgentSet(
        replaceDependencies(manifests, "@synara/cloud-agent-distribution", {
          ...manifests.find((manifest) => manifest.name === "@synara/cloud-agent-distribution")!
            .dependencies,
          "@synara/cloud-agent-runtime": "^0.2.0",
        }),
      ),
    ).toThrow(/exact semver/);
  });

  it.each([
    ["Runtime to Provider", "@synara/cloud-agent-runtime", "@synara/cloud-agent-provider-codex"],
    ["Runtime to Distribution", "@synara/cloud-agent-runtime", "@synara/cloud-agent-distribution"],
    ["Runtime to Testkit", "@synara/cloud-agent-runtime", "@synara/cloud-agent-testkit"],
    ["Provider API to Runtime", "@synara/cloud-agent-provider-api", "@synara/cloud-agent-runtime"],
    [
      "Protocol to Provider API",
      "@synara/cloud-agent-protocol",
      "@synara/cloud-agent-provider-api",
    ],
  ] as const)("rejects an extra %s internal edge", (_label, source, target) => {
    const manifests = validManifests();
    const versions = Object.fromEntries(
      manifests.map((manifest) => [manifest.name, manifest.version]),
    );
    expect(() =>
      validatePackedCloudAgentSet(
        replaceDependencies(manifests, source, {
          ...manifests.find((manifest) => manifest.name === source)!.dependencies,
          [target]: versions[target]!,
        }),
      ),
    ).toThrow(/internal dependencies must be exactly/);
  });

  it("rejects a missing required internal edge", () => {
    const manifests = validManifests();
    expect(() =>
      validatePackedCloudAgentSet(
        replaceDependencies(manifests, "@synara/cloud-agent-runtime", {
          "@synara/cloud-agent-protocol": "0.1.0",
        }),
      ),
    ).toThrow(/internal dependencies must be exactly/);
  });

  it("rejects internal runtime edges hidden outside dependencies", () => {
    const manifests = validManifests();
    expect(() =>
      validatePackedCloudAgentSet(
        manifests.map((manifest) =>
          manifest.name === "@synara/cloud-agent-runtime"
            ? Object.assign({}, manifest, {
                optionalDependencies: { "@synara/cloud-agent-provider-codex": "0.1.0" },
              })
            : manifest,
        ),
      ),
    ).toThrow(/not optionalDependencies/);
    expect(() =>
      validatePackedCloudAgentSet(
        manifests.map((manifest) =>
          manifest.name === "@synara/cloud-agent-provider-api"
            ? Object.assign({}, manifest, {
                peerDependencies: { "@synara/cloud-agent-runtime": "0.2.0" },
              })
            : manifest,
        ),
      ),
    ).toThrow(/not peerDependencies/);
    expect(() =>
      validatePackedCloudAgentSet(
        manifests.map((manifest) =>
          manifest.name === "@synara/cloud-agent-testkit"
            ? Object.assign({}, manifest, {
                devDependencies: { "@synara/cloud-agent-provider-codex": "0.1.0" },
              })
            : manifest,
        ),
      ),
    ).toThrow(/not devDependencies/);
  });

  it("requires the exact Claude Agent SDK production dependency", () => {
    const manifests = validManifests();
    const claude = manifests.find(
      (manifest) => manifest.name === "@synara/cloud-agent-provider-claude",
    )!;
    expect(() =>
      validatePackedCloudAgentSet(
        replaceDependencies(manifests, claude.name, {
          ...claude.dependencies,
          "@anthropic-ai/claude-agent-sdk": "^0.3.207",
        }),
      ),
    ).toThrow(/exclusively pin/);
  });

  it("rejects --skip-build so candidates always rebuild from source", () => {
    expect(() =>
      parseCloudAgentReleaseSmokeOptions(
        ["--allow-dirty", "--skip-build", "--output-dir", "candidate"],
        "/repo",
      ),
    ).toThrow(/must build every package from source/);
    expect(
      parseCloudAgentReleaseSmokeOptions(["--allow-dirty", "--output-dir", "candidate"], "/repo"),
    ).toEqual({ outputDirectory: "/repo/candidate", allowDirty: true });
  });

  it("binds candidate identity to unchanged tarball bits", () => {
    const packages = CLOUD_AGENT_PUBLIC_PACKAGES.map((name) => packed(name));
    expect(() =>
      assertSameCloudAgentBits(
        packages,
        packages.map((item) => ({ ...item })),
      ),
    ).not.toThrow();
    expect(() =>
      assertSameCloudAgentBits(packages, [
        ...packages.slice(0, -1),
        { ...packages.at(-1)!, sha256: `sha256:${"0".repeat(64)}` },
      ]),
    ).toThrow(/bits changed/);
    expect(cloudAgentCandidateDigest(packages)).toMatch(/^sha256:[0-9a-f]{64}$/u);
  });
});
