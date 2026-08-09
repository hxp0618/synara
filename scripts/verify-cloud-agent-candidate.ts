import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join, resolve } from "node:path";

const root = resolve(import.meta.dirname, "..");
const lockPath = join(root, "cloud-agent-candidate.lock.json");
const providerHostRequire = createRequire(join(root, "apps/provider-host/package.json"));
const distributionEntrypoint = providerHostRequire.resolve("@synara/cloud-agent-distribution");
const distributionRequire = createRequire(
  resolve(dirname(distributionEntrypoint), "../package.json"),
);
const packageNames = [
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
const lock = parseObject(readFileSync(lockPath, "utf8"), "Cloud Agent candidate lock");
assertCandidateLock(lock);
assertManifestReferences(lock);

for (const name of packageNames) {
  const artifact = packageArtifact(lock, name);
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

const standalone = parseObject(lock.standaloneRuntime, "standaloneRuntime");
const runtimeEntrypoint = resolve(dirname(distributionEntrypoint), "stdio.mjs");
const installedRuntimeDigest = sha256(readFileSync(runtimeEntrypoint));
if (installedRuntimeDigest !== standalone.sha256) {
  throw new Error(
    `Installed Distribution stdio digest ${installedRuntimeDigest} does not match ${String(standalone.sha256)}.`,
  );
}
if (!offline) {
  await verifyRemoteArtifact(
    "standalone runtime",
    requireString(standalone.url, "standaloneRuntime.url"),
    requireDigest(standalone.sha256, "standaloneRuntime.sha256"),
  );
}
process.stdout.write(
  `cloud-agent-candidate: verified ${offline ? "installed runtime closure" : "installed runtime closure and seven remote artifacts"} for ${String(lock.release && parseObject(lock.release, "release").candidateDigest)}\n`,
);

function assertCandidateLock(lock: Record<string, unknown>): void {
  if (lock.schemaVersion !== 1)
    throw new Error("Cloud Agent candidate lock schemaVersion must be 1.");
  const release = parseObject(lock.release, "release");
  if (
    release.repository !== "hxp0618/cloud-agents" ||
    release.tag !== "cloud-agent-m1-rc.1" ||
    !/^[0-9a-f]{40}$/u.test(String(release.sourceCommit))
  ) {
    throw new Error("Cloud Agent release identity is invalid.");
  }
  requireDigest(release.candidateDigest, "release.candidateDigest");
  const packages = parseObject(lock.packages, "packages");
  if (
    Object.keys(packages).length !== packageNames.length ||
    packageNames.some((name) => packages[name] === undefined)
  ) {
    throw new Error("Cloud Agent candidate lock must contain exactly seven public packages.");
  }
  for (const name of packageNames) packageArtifact(lock, name);
  const standalone = parseObject(lock.standaloneRuntime, "standaloneRuntime");
  requireReleaseUrl(standalone.url, "standaloneRuntime.url");
  requireDigest(standalone.sha256, "standaloneRuntime.sha256");
}

function assertManifestReferences(lock: Record<string, unknown>): void {
  const rootManifest = parseObject(
    JSON.parse(readFileSync(join(root, "package.json"), "utf8")),
    "root package.json",
  );
  const overrides = parseObject(rootManifest.overrides, "root overrides");
  for (const name of packageNames) {
    if (overrides[name] !== packageArtifact(lock, name).url) {
      throw new Error(`Root override for ${name} does not match the candidate lock.`);
    }
  }
  for (const [path, name] of [
    ["apps/provider-host/package.json", "@synara/cloud-agent-distribution"],
    ["packages/contracts/package.json", "@synara/cloud-agent-protocol"],
  ] as const) {
    const manifest = parseObject(JSON.parse(readFileSync(join(root, path), "utf8")), path);
    const dependencies = parseObject(manifest.dependencies, `${path} dependencies`);
    if (dependencies[name] !== packageArtifact(lock, name).url) {
      throw new Error(`${path} does not consume the candidate URL for ${name}.`);
    }
  }
}

function installedPackageManifest(name: (typeof packageNames)[number]): Record<string, unknown> {
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

function packageArtifact(lock: Record<string, unknown>, name: (typeof packageNames)[number]) {
  const packages = parseObject(lock.packages, "packages");
  const artifact = parseObject(packages[name], name);
  return {
    version: requireString(artifact.version, `${name}.version`),
    url: requireReleaseUrl(artifact.url, `${name}.url`),
    sha256: requireDigest(artifact.sha256, `${name}.sha256`),
  };
}

function requireReleaseUrl(value: unknown, label: string): string {
  const url = requireString(value, label);
  const prefix = "https://github.com/hxp0618/cloud-agents/releases/download/cloud-agent-m1-rc.1/";
  if (!url.startsWith(prefix) || url.slice(prefix.length).includes("/")) {
    throw new Error(`${label} must use the immutable cloud-agent-m1-rc.1 GitHub Release.`);
  }
  return url;
}

function requireDigest(value: unknown, label: string): `sha256:${string}` {
  const digest = requireString(value, label);
  if (!/^sha256:[0-9a-f]{64}$/u.test(digest)) throw new Error(`${label} is invalid.`);
  return digest as `sha256:${string}`;
}

function requireString(value: unknown, label: string): string {
  if (typeof value !== "string" || !value.trim()) throw new Error(`${label} is missing.`);
  return value;
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
