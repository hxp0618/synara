import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

export const CLOUD_AGENT_PUBLIC_PACKAGES = [
  "@synara/cloud-agent-protocol",
  "@synara/cloud-agent-provider-api",
  "@synara/cloud-agent-runtime",
  "@synara/cloud-agent-provider-codex",
  "@synara/cloud-agent-provider-claude",
  "@synara/cloud-agent-testkit",
  "@synara/cloud-agent-distribution",
] as const;

export type CloudAgentPublicPackageName = (typeof CLOUD_AGENT_PUBLIC_PACKAGES)[number];

type JSONRecord = Record<string, unknown>;

const DEPENDENCY_SECTIONS = [
  "dependencies",
  "optionalDependencies",
  "peerDependencies",
  "devDependencies",
] as const;
const LOCAL_PROTOCOL = /^(?:workspace|catalog|file|link|portal):/u;
const EXACT_SEMVER =
  /^(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/u;
const PUBLIC_PACKAGE_SET = new Set<string>(CLOUD_AGENT_PUBLIC_PACKAGES);
const CLAUDE_AGENT_SDK = "@anthropic-ai/claude-agent-sdk";
const CLAUDE_AGENT_SDK_VERSION = "0.3.207";
const EXPECTED_INTERNAL_DEPENDENCIES = {
  "@synara/cloud-agent-protocol": [],
  "@synara/cloud-agent-provider-api": ["@synara/cloud-agent-protocol"],
  "@synara/cloud-agent-runtime": [
    "@synara/cloud-agent-protocol",
    "@synara/cloud-agent-provider-api",
  ],
  "@synara/cloud-agent-provider-codex": ["@synara/cloud-agent-provider-api"],
  "@synara/cloud-agent-provider-claude": ["@synara/cloud-agent-provider-api"],
  "@synara/cloud-agent-testkit": [
    "@synara/cloud-agent-protocol",
    "@synara/cloud-agent-provider-api",
  ],
  "@synara/cloud-agent-distribution": [
    "@synara/cloud-agent-protocol",
    "@synara/cloud-agent-provider-api",
    "@synara/cloud-agent-runtime",
    "@synara/cloud-agent-provider-codex",
    "@synara/cloud-agent-provider-claude",
  ],
} as const satisfies Readonly<
  Record<CloudAgentPublicPackageName, ReadonlyArray<CloudAgentPublicPackageName>>
>;

export type PackedCloudAgentPackage = {
  readonly name: CloudAgentPublicPackageName;
  readonly version: string;
  readonly filename: string;
  readonly sha256: string;
};

export type CloudAgentReleaseSmokeOptions = {
  readonly outputDirectory: string;
  readonly allowDirty: boolean;
};

export function cloudAgentTarballClosure(
  target: CloudAgentPublicPackageName,
): ReadonlyArray<CloudAgentPublicPackageName> {
  const reachable = new Set<CloudAgentPublicPackageName>();
  const visit = (name: CloudAgentPublicPackageName): void => {
    if (reachable.has(name)) return;
    reachable.add(name);
    for (const dependency of EXPECTED_INTERNAL_DEPENDENCIES[name]) visit(dependency);
  };
  visit(target);
  return CLOUD_AGENT_PUBLIC_PACKAGES.filter((name) => reachable.has(name));
}

export function cloudAgentStableImportSpecifiers(
  target: CloudAgentPublicPackageName,
): ReadonlyArray<string> {
  switch (target) {
    case "@synara/cloud-agent-provider-api":
      return [target, `${target}/internal`];
    case "@synara/cloud-agent-runtime":
      return [target, `${target}/node`];
    case "@synara/cloud-agent-distribution":
      return [target, `${target}/schemas`, `${target}/schemas/cloud-agent-envelope-v2`];
    default:
      return [target];
  }
}

export function parseCloudAgentReleaseSmokeOptions(
  args: ReadonlyArray<string>,
  cwd = process.cwd(),
): CloudAgentReleaseSmokeOptions {
  let outputDirectory: string | undefined;
  let allowDirty = false;
  for (let index = 0; index < args.length; index += 1) {
    const value = args[index];
    if (value === "--skip-build") {
      throw new Error(
        "--skip-build is not supported: a release candidate must build every package from source before packing.",
      );
    }
    if (value === "--allow-dirty") {
      allowDirty = true;
      continue;
    }
    if (value === "--output-dir") {
      const candidate = args[index + 1];
      if (!candidate || candidate.startsWith("--")) {
        throw new Error("--output-dir requires a directory path.");
      }
      outputDirectory = candidate;
      index += 1;
      continue;
    }
    throw new Error(`Unknown argument: ${String(value)}`);
  }
  if (!outputDirectory) {
    throw new Error(
      "Usage: node scripts/cloud-agent-release-smoke.ts --output-dir <new-directory>",
    );
  }
  return { outputDirectory: resolve(cwd, outputDirectory), allowDirty };
}

export function validatePackedCloudAgentManifest(manifest: JSONRecord): void {
  const name = requireString(manifest.name, "package name");
  const version = requireString(manifest.version, `${name} version`);
  if (!PUBLIC_PACKAGE_SET.has(name)) {
    throw new Error(`Unexpected Cloud Agent package: ${name}.`);
  }
  if (!EXACT_SEMVER.test(version)) {
    throw new Error(`${name} must use an exact semver version, found ${version}.`);
  }
  if (manifest.private === true) throw new Error(`${name} is still private.`);
  if (
    name === "@synara/cloud-agent-runtime" &&
    isRecord(manifest.exports) &&
    manifest.exports["./legacy-provider-host"] !== undefined
  ) {
    throw new Error("Cloud Agent Runtime still exports the legacy Provider facade.");
  }

  for (const section of DEPENDENCY_SECTIONS) {
    const dependencies = manifest[section];
    if (dependencies === undefined) continue;
    if (!isRecord(dependencies)) throw new Error(`${name} ${section} must be an object.`);
    for (const [dependency, value] of Object.entries(dependencies)) {
      const specifier = requireString(value, `${name} ${section}.${dependency}`);
      if (LOCAL_PROTOCOL.test(specifier)) {
        throw new Error(`${name} ${section}.${dependency} uses local protocol ${specifier}.`);
      }
      if (dependency.startsWith("@synara/") && !PUBLIC_PACKAGE_SET.has(dependency)) {
        throw new Error(`${name} depends on unpublished private package ${dependency}.`);
      }
      if (PUBLIC_PACKAGE_SET.has(dependency) && !EXACT_SEMVER.test(specifier)) {
        throw new Error(`${name} must pin ${dependency} to exact semver, found ${specifier}.`);
      }
    }
  }
}

export function validatePackedCloudAgentSet(manifests: ReadonlyArray<JSONRecord>): void {
  if (manifests.length !== CLOUD_AGENT_PUBLIC_PACKAGES.length) {
    throw new Error(
      `Expected ${CLOUD_AGENT_PUBLIC_PACKAGES.length} packed Cloud Agent manifests, found ${manifests.length}.`,
    );
  }
  const versions = new Map<string, string>();
  for (const manifest of manifests) {
    validatePackedCloudAgentManifest(manifest);
    const name = requireString(manifest.name, "package name");
    if (versions.has(name)) throw new Error(`Packed Cloud Agent package ${name} is duplicated.`);
    versions.set(name, requireString(manifest.version, `${name} version`));
  }
  for (const expected of CLOUD_AGENT_PUBLIC_PACKAGES) {
    if (!versions.has(expected))
      throw new Error(`Packed Cloud Agent package ${expected} is missing.`);
  }
  for (const manifest of manifests) {
    const name = requireString(manifest.name, "package name");
    const dependencies = isRecord(manifest.dependencies) ? manifest.dependencies : {};
    for (const [dependency, expectedVersion] of versions) {
      if (dependencies[dependency] !== undefined && dependencies[dependency] !== expectedVersion) {
        throw new Error(
          `${name} pins ${dependency} to ${String(dependencies[dependency])}; packed version is ${expectedVersion}.`,
        );
      }
    }
  }
  const byName = new Map(
    manifests.map((manifest) => [requireString(manifest.name, "package name"), manifest]),
  );
  for (const name of CLOUD_AGENT_PUBLIC_PACKAGES) {
    const manifest = byName.get(name)!;
    const dependencies = dependencyRecord(manifest.dependencies);
    const actualInternal = Object.keys(dependencies)
      .filter((dependency) => PUBLIC_PACKAGE_SET.has(dependency))
      .toSorted();
    const expectedInternal = [...EXPECTED_INTERNAL_DEPENDENCIES[name]].toSorted();
    if (JSON.stringify(actualInternal) !== JSON.stringify(expectedInternal)) {
      throw new Error(
        `${name} internal dependencies must be exactly [${expectedInternal.join(", ")}], found [${actualInternal.join(", ")}].`,
      );
    }
    for (const dependency of expectedInternal) {
      const expectedVersion = versions.get(dependency)!;
      if (dependencies[dependency] !== expectedVersion) {
        throw new Error(
          `${name} must pin ${dependency} to packed version ${expectedVersion}, found ${String(dependencies[dependency])}.`,
        );
      }
    }
    for (const section of [
      "optionalDependencies",
      "peerDependencies",
      "devDependencies",
    ] as const) {
      const misplacedInternal = Object.keys(dependencyRecord(manifest[section])).find(
        (dependency) => PUBLIC_PACKAGE_SET.has(dependency),
      );
      if (misplacedInternal) {
        throw new Error(
          `${name} must declare internal dependency ${misplacedInternal} in dependencies, not ${section}.`,
        );
      }
    }
  }

  for (const [name, manifest] of byName) {
    for (const section of DEPENDENCY_SECTIONS) {
      const sdkVersion = dependencyRecord(manifest[section])[CLAUDE_AGENT_SDK];
      if (sdkVersion === undefined) continue;
      if (name !== "@synara/cloud-agent-provider-claude") {
        throw new Error(`${name} must not carry the Claude Agent SDK.`);
      }
      if (section !== "dependencies") {
        throw new Error(`Claude Provider must declare ${CLAUDE_AGENT_SDK} in dependencies only.`);
      }
      if (sdkVersion !== CLAUDE_AGENT_SDK_VERSION) {
        throw new Error(
          `Claude Provider must exclusively pin ${CLAUDE_AGENT_SDK} ${CLAUDE_AGENT_SDK_VERSION}.`,
        );
      }
    }
  }
  const claudeDependencies = dependencyRecord(
    byName.get("@synara/cloud-agent-provider-claude")!.dependencies,
  );
  if (claudeDependencies[CLAUDE_AGENT_SDK] !== CLAUDE_AGENT_SDK_VERSION) {
    throw new Error(
      `Claude Provider must exclusively pin ${CLAUDE_AGENT_SDK} ${CLAUDE_AGENT_SDK_VERSION}.`,
    );
  }
}

export function sha256File(path: string): string {
  return `sha256:${createHash("sha256").update(readFileSync(path)).digest("hex")}`;
}

export function assertSameCloudAgentBits(
  before: ReadonlyArray<PackedCloudAgentPackage>,
  after: ReadonlyArray<PackedCloudAgentPackage>,
): void {
  if (
    JSON.stringify(normalizePackedCloudAgentPackages(before)) !==
    JSON.stringify(normalizePackedCloudAgentPackages(after))
  ) {
    throw new Error("Cloud Agent tarball bits changed after external smoke validation.");
  }
}

function normalizePackedCloudAgentPackages(items: ReadonlyArray<PackedCloudAgentPackage>) {
  return items
    .map(({ name, version, filename, sha256 }) => ({ name, version, filename, sha256 }))
    .toSorted((left, right) => left.name.localeCompare(right.name));
}

export function cloudAgentCandidateDigest(
  packages: ReadonlyArray<PackedCloudAgentPackage>,
): string {
  const identity = packages
    .map(({ name, version, sha256 }) => `${name}@${version} ${sha256}`)
    .toSorted()
    .join("\n");
  return `sha256:${createHash("sha256").update(`${identity}\n`).digest("hex")}`;
}

function requireString(value: unknown, label: string): string {
  if (typeof value !== "string" || value.trim() === "") throw new Error(`${label} is missing.`);
  return value.trim();
}

function isRecord(value: unknown): value is JSONRecord {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function dependencyRecord(value: unknown): JSONRecord {
  return isRecord(value) ? value : {};
}
