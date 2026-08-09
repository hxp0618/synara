import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join, resolve } from "node:path";

import {
  CLOUD_AGENT_PACKAGE_NAMES,
  validateCloudAgentCandidateLock,
} from "../deploy/worker/cloud-agent-candidate.mjs";

const root = resolve(import.meta.dirname, "..");
const lockPath = join(root, "cloud-agent-candidate.lock.json");
const providerHostRequire = createRequire(join(root, "apps/provider-host/package.json"));
const distributionEntrypoint = providerHostRequire.resolve("@synara/cloud-agent-distribution");
const distributionRequire = createRequire(
  resolve(dirname(distributionEntrypoint), "../package.json"),
);
type PackageName =
  | "@synara/cloud-agent-protocol"
  | "@synara/cloud-agent-provider-api"
  | "@synara/cloud-agent-runtime"
  | "@synara/cloud-agent-provider-codex"
  | "@synara/cloud-agent-provider-claude"
  | "@synara/cloud-agent-testkit"
  | "@synara/cloud-agent-distribution";
type CandidateArtifact = {
  name: PackageName;
  version: string;
  url: string;
  sha256: `sha256:${string}`;
};
type ValidatedCandidate = {
  candidateDigest: `sha256:${string}`;
  standaloneRuntime: { url: string; sha256: `sha256:${string}` };
  packages: CandidateArtifact[];
};
const packageNames = CLOUD_AGENT_PACKAGE_NAMES as readonly PackageName[];
const expectedPackageNames = [
  "@synara/cloud-agent-protocol",
  "@synara/cloud-agent-provider-api",
  "@synara/cloud-agent-runtime",
  "@synara/cloud-agent-provider-codex",
  "@synara/cloud-agent-provider-claude",
  "@synara/cloud-agent-testkit",
  "@synara/cloud-agent-distribution",
] as const;
const offline = process.argv.slice(2).includes("--offline");

if (!existsSync(lockPath)) {
  throw new Error(
    "cloud-agent-candidate.lock.json is missing; do not resolve the host until the immutable GitHub RC is published.",
  );
}
const candidate = validateCloudAgentCandidateLock(
  readFileSync(lockPath, "utf8"),
) as ValidatedCandidate;
if (
  packageNames.length !== expectedPackageNames.length ||
  expectedPackageNames.some((name) => !packageNames.includes(name))
) {
  throw new Error("Shared Cloud Agent candidate validator package catalog is incomplete.");
}
assertManifestReferences(candidate);

for (const name of packageNames) {
  const artifact = candidate.packages.find((item) => item.name === name);
  if (!artifact) throw new Error(`Validated candidate omitted ${name}.`);
  if (name !== "@synara/cloud-agent-testkit") {
    const installedManifest = installedPackageManifest(name);
    if (installedManifest.version !== artifact.version) {
      throw new Error(
        `${name} installed version ${String(installedManifest.version)} does not match candidate ${artifact.version}.`,
      );
    }
  }
  if (!offline) await verifyRemoteArtifact(name, artifact.url, artifact.sha256);
}

const standalone = candidate.standaloneRuntime;
const runtimeEntrypoint = resolve(dirname(distributionEntrypoint), "stdio.mjs");
const installedRuntimeDigest = sha256(readFileSync(runtimeEntrypoint));
if (installedRuntimeDigest !== standalone.sha256) {
  throw new Error(
    `Installed Distribution stdio digest ${installedRuntimeDigest} does not match ${String(standalone.sha256)}.`,
  );
}
if (!offline) {
  await verifyRemoteArtifact("standalone runtime", standalone.url, standalone.sha256);
}
process.stdout.write(
  `cloud-agent-candidate: verified ${offline ? "installed runtime closure" : "installed runtime closure and seven remote artifacts"} for ${candidate.candidateDigest}\n`,
);

function assertManifestReferences(candidate: ValidatedCandidate): void {
  const rootManifest = parseObject(
    JSON.parse(readFileSync(join(root, "package.json"), "utf8")),
    "root package.json",
  );
  const overrides = parseObject(rootManifest.overrides, "root overrides");
  for (const name of packageNames) {
    const artifact = candidate.packages.find((item) => item.name === name);
    if (!artifact || overrides[name] !== artifact.url) {
      throw new Error(`Root override for ${name} does not match the candidate lock.`);
    }
  }
  for (const [path, name] of [
    ["apps/provider-host/package.json", "@synara/cloud-agent-distribution"],
    ["packages/contracts/package.json", "@synara/cloud-agent-protocol"],
  ] as const) {
    const manifest = parseObject(JSON.parse(readFileSync(join(root, path), "utf8")), path);
    const dependencies = parseObject(manifest.dependencies, `${path} dependencies`);
    const artifact = candidate.packages.find((item) => item.name === name);
    if (!artifact || dependencies[name] !== artifact.url) {
      throw new Error(`${path} does not consume the candidate URL for ${name}.`);
    }
  }
}

function installedPackageManifest(name: PackageName): Record<string, unknown> {
  const entrypoint =
    name === "@synara/cloud-agent-distribution"
      ? distributionEntrypoint
      : distributionRequire.resolve(name);
  let directory = dirname(entrypoint);
  while (directory !== dirname(directory)) {
    const manifestPath = join(directory, "package.json");
    if (existsSync(manifestPath)) {
      const manifest = parseObject(JSON.parse(readFileSync(manifestPath, "utf8")), manifestPath);
      if (manifest.name === name) return manifest;
    }
    directory = dirname(directory);
  }
  throw new Error(`Could not locate installed package.json for ${name}.`);
}

async function verifyRemoteArtifact(name: string, url: string, expected: string): Promise<void> {
  let failure: unknown;
  for (let attempt = 1; attempt <= 3; attempt += 1) {
    try {
      const response = await fetch(url, { redirect: "follow" });
      if (!response.ok) {
        throw new Error(`${name} candidate asset returned HTTP ${response.status}.`);
      }
      const actual = sha256(new Uint8Array(await response.arrayBuffer()));
      if (actual !== expected) throw new Error(`${name} candidate asset digest mismatch.`);
      return;
    } catch (error) {
      failure = error;
    }
  }
  throw failure;
}

function parseObject(value: unknown, label: string): Record<string, unknown>;
function parseObject(value: string, label: string): Record<string, unknown>;
function parseObject(value: unknown, label: string): Record<string, unknown> {
  const parsed = typeof value === "string" ? (JSON.parse(value) as unknown) : value;
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error(`${label} must be an object.`);
  }
  return parsed as Record<string, unknown>;
}

function sha256(value: Uint8Array): `sha256:${string}` {
  return `sha256:${createHash("sha256").update(value).digest("hex")}`;
}
