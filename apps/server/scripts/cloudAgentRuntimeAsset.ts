import { createHash } from "node:crypto";
import { chmodSync, copyFileSync, existsSync, readFileSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { resolveCloudAgentRuntimeExecutable } from "@synara/cloud-agent-distribution";
import { CLOUD_AGENT_ENVELOPE_V2_SCHEMA } from "@synara/cloud-agent-distribution/schemas";

export const CLOUD_AGENT_RUNTIME_ASSET_NAME = "cloudAgentRuntimeChild.mjs";
export const CLOUD_AGENT_CANDIDATE_LOCK_NAME = "cloud-agent-candidate.lock.json";

const CLOUD_AGENT_PACKAGE_NAMES = [
  "@synara/cloud-agent-distribution",
  "@synara/cloud-agent-protocol",
  "@synara/cloud-agent-provider-api",
  "@synara/cloud-agent-provider-claude",
  "@synara/cloud-agent-provider-codex",
  "@synara/cloud-agent-runtime",
  "@synara/cloud-agent-testkit",
] as const;

export interface CandidateLock {
  readonly schemaVersion: 1;
  readonly kind: "synara-cloud-agent-release-candidate-lock";
  readonly source: { readonly repository: string; readonly commit: string };
  readonly release: { readonly tag: string; readonly baseUrl: string };
  readonly candidateSha256: string;
  readonly standaloneRuntime: {
    readonly filename: string;
    readonly url: string;
    readonly sha256: string;
  };
  readonly packages: ReadonlyArray<{
    readonly name: (typeof CLOUD_AGENT_PACKAGE_NAMES)[number];
    readonly version: string;
    readonly filename: string;
    readonly url: string;
    readonly sha256: string;
  }>;
}

export function resolveInstalledCloudAgentRuntimeAsset(): string {
  const manifest = fileURLToPath(
    import.meta.resolve("@synara/cloud-agent-distribution/manifest.json"),
  );
  return resolveCloudAgentRuntimeExecutable(dirname(manifest));
}

export function readCloudAgentCandidateLock(
  repoRoot: string,
  options: { readonly required?: boolean } = {},
): CandidateLock | undefined {
  const lockPath = join(repoRoot, CLOUD_AGENT_CANDIDATE_LOCK_NAME);
  if (!existsSync(lockPath)) {
    if (options.required) {
      throw new Error(`Missing Cloud Agent candidate lock: ${lockPath}.`);
    }
    return undefined;
  }
  const value = JSON.parse(readFileSync(lockPath, "utf8")) as unknown;
  if (
    !isRecord(value) ||
    value.schemaVersion !== 1 ||
    value.kind !== "synara-cloud-agent-release-candidate-lock"
  ) {
    throw new Error("Cloud Agent candidate lock header is invalid.");
  }
  const source = isRecord(value.source) ? value.source : undefined;
  const release = isRecord(value.release) ? value.release : undefined;
  if (
    !source ||
    source.repository !== "https://github.com/hxp0618/cloud-agents" ||
    typeof source.commit !== "string" ||
    !/^[0-9a-f]{40}$/u.test(source.commit) ||
    !release ||
    release.tag !== "cloud-agent-m1-rc.1" ||
    release.baseUrl !==
      "https://github.com/hxp0618/cloud-agents/releases/download/cloud-agent-m1-rc.1" ||
    typeof value.candidateSha256 !== "string" ||
    !/^sha256:[0-9a-f]{64}$/u.test(value.candidateSha256)
  ) {
    throw new Error("Cloud Agent candidate lock source or Release identity is invalid.");
  }
  const standalone = isRecord(value.standaloneRuntime) ? value.standaloneRuntime : undefined;
  if (
    !standalone ||
    standalone.filename !== "cloud-agent-runtime-standalone.mjs" ||
    standalone.url !== `${release.baseUrl}/cloud-agent-runtime-standalone.mjs` ||
    typeof standalone.sha256 !== "string" ||
    !/^sha256:[0-9a-f]{64}$/u.test(standalone.sha256)
  ) {
    throw new Error("Cloud Agent candidate lock standalone Runtime is invalid.");
  }
  if (
    !Array.isArray(value.packages) ||
    value.packages.length !== CLOUD_AGENT_PACKAGE_NAMES.length
  ) {
    throw new Error("Cloud Agent candidate lock must contain all seven package artifacts.");
  }
  const packages = new Map<string, Record<string, unknown>>();
  for (const item of value.packages) {
    if (!isRecord(item) || typeof item.name !== "string" || packages.has(item.name)) {
      throw new Error("Cloud Agent candidate lock package entries are invalid or duplicated.");
    }
    packages.set(item.name, item);
  }
  for (const name of CLOUD_AGENT_PACKAGE_NAMES) {
    const item = packages.get(name);
    const expectedVersion = name === "@synara/cloud-agent-runtime" ? "0.2.0-rc.1" : "0.1.0-rc.1";
    const expectedFilename = `${name.slice(1).replace("/", "-")}-${expectedVersion}.tgz`;
    if (
      !item ||
      item.version !== expectedVersion ||
      item.filename !== expectedFilename ||
      item.url !== `${release.baseUrl}/${item.filename}` ||
      typeof item.sha256 !== "string" ||
      !/^sha256:[0-9a-f]{64}$/u.test(item.sha256)
    ) {
      throw new Error(`Cloud Agent candidate lock package ${name} is invalid.`);
    }
  }
  return value as unknown as CandidateLock;
}

export function assertCloudAgentDependencyUrls(
  dependencies: Readonly<Record<string, unknown>>,
  lock: CandidateLock,
): void {
  for (const artifact of lock.packages) {
    if (artifact.name === "@synara/cloud-agent-testkit") continue;
    if (dependencies[artifact.name] !== artifact.url) {
      throw new Error(
        `Cloud Agent dependency ${artifact.name} does not match the candidate lock URL.`,
      );
    }
  }
}

export function installCloudAgentRuntimeAsset(input: {
  readonly repoRoot: string;
  readonly targetDirectory: string;
}): string {
  assertCloudAgentPublicSchema();
  const source = resolveInstalledCloudAgentRuntimeAsset();
  const target = join(input.targetDirectory, CLOUD_AGENT_RUNTIME_ASSET_NAME);
  copyFileSync(source, target);
  if (process.platform !== "win32") chmodSync(target, 0o755);
  const expectedSha256 = readCloudAgentCandidateLock(input.repoRoot)?.standaloneRuntime.sha256;
  assertCloudAgentRuntimeAsset({
    assetPath: target,
    ...(expectedSha256 ? { expectedSha256 } : {}),
  });
  return target;
}

export function assertCloudAgentRuntimeAsset(input: {
  readonly assetPath: string;
  readonly expectedSha256?: string;
}): void {
  const stats = statSync(input.assetPath);
  if (!stats.isFile() || stats.size === 0) {
    throw new Error(`Cloud Agent Runtime asset is missing or empty: ${input.assetPath}.`);
  }
  if (process.platform !== "win32" && (stats.mode & 0o111) === 0) {
    throw new Error(`Cloud Agent Runtime asset is not executable: ${input.assetPath}.`);
  }
  if (!input.expectedSha256) return;
  const actual = `sha256:${createHash("sha256").update(readFileSync(input.assetPath)).digest("hex")}`;
  if (actual !== input.expectedSha256) {
    throw new Error(
      `Cloud Agent Runtime asset digest mismatch: expected ${input.expectedSha256}, got ${actual}.`,
    );
  }
}

export function assertCloudAgentPublicSchema(): void {
  if (
    CLOUD_AGENT_ENVELOPE_V2_SCHEMA.$id !==
    "https://schemas.synara.dev/cloud-agent/envelope-v2.schema.json"
  ) {
    throw new Error("Cloud Agent Protocol v2 schema export is incompatible.");
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
