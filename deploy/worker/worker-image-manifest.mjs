import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { parseArgs } from "node:util";

const SCHEMA_VERSION = 1;
const PROVIDER_TOOLS_LOCKFILE_PATH = "/opt/synara/provider-tools/package-lock.json";
const PROVIDER_HOST_LOCKFILE_PATH = "/opt/synara/provider-host/bun.lock";
const WORKER_APK_LOCKFILE_PATH = "/opt/synara/worker-apk-packages.lock";
const PROVIDER_TOOLS_SBOM_PATH = "/opt/synara/provider-tools.spdx.json";

const REQUIRED_BASE_IMAGES = new Set(["agentd-build", "provider-host-build", "worker-runtime"]);
const REQUIRED_PROVIDER_TOOLS = [
  { provider: "claudeAgent", kind: "cli", package: "@anthropic-ai/claude-code" },
  { provider: "codex", kind: "cli", package: "@openai/codex" },
];
const REQUIRED_APK_PACKAGES = new Set([
  "bash",
  "ca-certificates",
  "git",
  "jq",
  "openssh-client-default",
  "ripgrep",
]);

function invariant(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}

function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function parseJSONObject(value, label) {
  let parsed;
  try {
    parsed = JSON.parse(value);
  } catch (error) {
    throw new Error(
      `${label} is not valid JSON: ${error instanceof Error ? error.message : String(error)}`,
    );
  }
  invariant(isRecord(parsed), `${label} must be a JSON object`);
  return parsed;
}

export function sha256Hex(value) {
  return createHash("sha256").update(value).digest("hex");
}

function sortedObject(value) {
  if (Array.isArray(value)) {
    return value.map(sortedObject);
  }
  if (!isRecord(value)) {
    return value;
  }
  return Object.fromEntries(
    Object.keys(value)
      .sort()
      .map((key) => [key, sortedObject(value[key])]),
  );
}

function canonicalJSON(value) {
  return `${JSON.stringify(sortedObject(value), null, 2)}\n`;
}

function normalizeSourceVersion(value) {
  const version = String(value ?? "").trim();
  invariant(version.length > 0 && version.length <= 128, "source version must be 1-128 characters");
  invariant(
    /^[0-9A-Za-z][0-9A-Za-z._+\-]*$/.test(version),
    "source version contains unsupported characters",
  );
  return version;
}

function normalizeGitSHA(value) {
  const gitSHA = String(value ?? "").trim();
  invariant(
    /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(gitSHA),
    "source Git SHA must be a full lowercase hexadecimal object ID",
  );
  return gitSHA;
}

function normalizeArchitecture(value) {
  const architecture = String(value ?? "").trim();
  invariant(
    architecture === "amd64" || architecture === "arm64",
    "worker architecture must be amd64 or arm64",
  );
  return architecture;
}

function normalizeSourceDateEpoch(value) {
  const text = String(value ?? "").trim();
  invariant(/^(?:0|[1-9][0-9]*)$/.test(text), "SOURCE_DATE_EPOCH must be a non-negative integer");
  const milliseconds = Number(text) * 1000;
  invariant(Number.isSafeInteger(milliseconds), "SOURCE_DATE_EPOCH is outside the supported range");
  const created = new Date(milliseconds);
  invariant(!Number.isNaN(created.getTime()), "SOURCE_DATE_EPOCH is invalid");
  return created.toISOString();
}

function normalizeBaseImages(values) {
  invariant(
    Array.isArray(values) && values.length === REQUIRED_BASE_IMAGES.size,
    "exactly three Worker base images are required",
  );
  const result = [];
  const names = new Set();
  for (const value of values) {
    const separator = value.indexOf("=");
    invariant(separator > 0, `base image ${value} must use name=reference`);
    const name = value.slice(0, separator);
    const reference = value.slice(separator + 1);
    invariant(REQUIRED_BASE_IMAGES.has(name), `unknown Worker base image ${name}`);
    invariant(!names.has(name), `duplicate Worker base image ${name}`);
    invariant(
      /^[^\s@]+@sha256:[0-9a-f]{64}$/.test(reference),
      `${name} must use an immutable sha256 image reference`,
    );
    names.add(name);
    result.push({ name, reference });
  }
  for (const name of REQUIRED_BASE_IMAGES) {
    invariant(names.has(name), `missing Worker base image ${name}`);
  }
  return result.sort((left, right) => left.name.localeCompare(right.name));
}

function packageVersion(lockfile, packageName) {
  const packages = lockfile.packages;
  invariant(isRecord(packages), "Provider tools package-lock is missing packages");
  const entry = packages[`node_modules/${packageName}`];
  invariant(isRecord(entry), `Provider tools package-lock is missing ${packageName}`);
  const version = String(entry.version ?? "").trim();
  invariant(version.length > 0, `Provider tools package-lock has no version for ${packageName}`);
  const root = packages[""];
  invariant(
    isRecord(root) && isRecord(root.dependencies),
    "Provider tools package-lock is missing root dependencies",
  );
  invariant(
    root.dependencies[packageName] === version,
    `Provider tools dependency ${packageName} is not exactly locked to ${version}`,
  );
  return version;
}

function providerRuntimes(providerToolsLockfile, providerHostPackageJSON) {
  const lockfile = parseJSONObject(providerToolsLockfile, "Provider tools package-lock");
  const providerHostPackage = parseJSONObject(
    providerHostPackageJSON,
    "Provider Host package.json",
  );
  invariant(
    isRecord(providerHostPackage.dependencies),
    "Provider Host package.json is missing dependencies",
  );
  const sdkVersion = String(
    providerHostPackage.dependencies["@anthropic-ai/claude-agent-sdk"] ?? "",
  ).trim();
  invariant(
    /^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$/.test(sdkVersion),
    "Claude Agent SDK must use an exact version",
  );
  const runtimes = REQUIRED_PROVIDER_TOOLS.map((runtime) => ({
    ...runtime,
    version: packageVersion(lockfile, runtime.package),
  }));
  runtimes.push({
    provider: "claudeAgent",
    kind: "sdk",
    package: "@anthropic-ai/claude-agent-sdk",
    version: sdkVersion,
  });
  return runtimes.sort((left, right) =>
    `${left.provider}:${left.kind}:${left.package}`.localeCompare(
      `${right.provider}:${right.kind}:${right.package}`,
    ),
  );
}

function validateAPKLockfile(value) {
  const names = new Set();
  const entries = [];
  for (const rawLine of value.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (line === "" || line.startsWith("#")) {
      continue;
    }
    const match = /^([a-z0-9][a-z0-9+_.-]*)=([^\s=]+)$/.exec(line);
    invariant(match !== null, `Worker APK lock entry ${line} is invalid`);
    invariant(!names.has(match[1]), `Worker APK lock contains duplicate package ${match[1]}`);
    names.add(match[1]);
    entries.push(line);
  }
  for (const packageName of REQUIRED_APK_PACKAGES) {
    invariant(names.has(packageName), `Worker APK lock is missing ${packageName}`);
  }
  invariant(
    entries.length > REQUIRED_APK_PACKAGES.size,
    "Worker APK lock must include the resolved dependency closure",
  );
}

function normalizeProviderToolsSBOM(rawValue, { lockfileSHA256, created, architecture, runtimes }) {
  const document = parseJSONObject(rawValue, "Provider tools SPDX SBOM");
  invariant(document.spdxVersion === "SPDX-2.3", "Provider tools SBOM must use SPDX 2.3");
  invariant(Array.isArray(document.packages), "Provider tools SBOM is missing packages");
  const packages = new Set();
  for (const entry of document.packages) {
    if (
      isRecord(entry) &&
      typeof entry.name === "string" &&
      typeof entry.versionInfo === "string"
    ) {
      packages.add(`${entry.name}\x00${entry.versionInfo}`);
    }
  }
  for (const runtime of runtimes.filter((entry) => entry.kind === "cli")) {
    invariant(
      packages.has(`${runtime.package}\x00${runtime.version}`),
      `Provider tools SBOM does not describe ${runtime.package}@${runtime.version}`,
    );
  }
  document.name = "synara-worker-provider-tools";
  document.documentNamespace = `https://synara.dev/spdx/worker-provider-tools/${lockfileSHA256}/linux-${architecture}`;
  document.creationInfo = {
    created,
    creators: ["Tool: synara-worker-image-manifest/1"],
  };
  if (Array.isArray(document.documentDescribes)) {
    document.documentDescribes = [...document.documentDescribes].sort();
  }
  document.packages = [...document.packages].sort((left, right) =>
    `${left?.name ?? ""}:${left?.versionInfo ?? ""}:${left?.SPDXID ?? ""}`.localeCompare(
      `${right?.name ?? ""}:${right?.versionInfo ?? ""}:${right?.SPDXID ?? ""}`,
    ),
  );
  if (Array.isArray(document.relationships)) {
    document.relationships = [...document.relationships].sort((left, right) =>
      `${left?.spdxElementId ?? ""}:${left?.relationshipType ?? ""}:${left?.relatedSpdxElement ?? ""}`.localeCompare(
        `${right?.spdxElementId ?? ""}:${right?.relationshipType ?? ""}:${right?.relatedSpdxElement ?? ""}`,
      ),
    );
  }
  return canonicalJSON(document);
}

export function buildWorkerImageArtifacts({
  version,
  gitSHA,
  sourceDateEpoch,
  architecture,
  baseImages,
  providerToolsLockfile,
  providerHostLockfile,
  providerHostPackageJSON,
  workerAPKLockfile,
  rawProviderToolsSBOM,
}) {
  const sourceVersion = normalizeSourceVersion(version);
  const sourceGitSHA = normalizeGitSHA(gitSHA);
  const workerArchitecture = normalizeArchitecture(architecture);
  const created = normalizeSourceDateEpoch(sourceDateEpoch);
  const normalizedBaseImages = normalizeBaseImages(baseImages);
  validateAPKLockfile(workerAPKLockfile);
  const runtimes = providerRuntimes(providerToolsLockfile, providerHostPackageJSON);
  const providerToolsLockfileSHA256 = sha256Hex(providerToolsLockfile);
  const providerToolsSBOM = normalizeProviderToolsSBOM(rawProviderToolsSBOM, {
    lockfileSHA256: providerToolsLockfileSHA256,
    created,
    architecture: workerArchitecture,
    runtimes,
  });
  const manifest = {
    schemaVersion: SCHEMA_VERSION,
    source: { version: sourceVersion, gitSha: sourceGitSHA },
    platform: { os: "linux", architecture: workerArchitecture },
    baseImages: normalizedBaseImages,
    lockfiles: [
      {
        name: "provider-host-bun",
        path: PROVIDER_HOST_LOCKFILE_PATH,
        sha256: sha256Hex(providerHostLockfile),
      },
      {
        name: "provider-tools-npm",
        path: PROVIDER_TOOLS_LOCKFILE_PATH,
        sha256: providerToolsLockfileSHA256,
      },
      {
        name: "worker-apk",
        path: WORKER_APK_LOCKFILE_PATH,
        sha256: sha256Hex(workerAPKLockfile),
      },
    ],
    providerRuntimes: runtimes,
    sboms: [
      {
        name: "provider-tools",
        format: "spdx-json",
        path: PROVIDER_TOOLS_SBOM_PATH,
        sha256: sha256Hex(providerToolsSBOM),
      },
    ],
  };
  return {
    manifest,
    manifestJSON: canonicalJSON(manifest),
    providerToolsSBOM,
  };
}

async function writeArtifact(filePath, contents) {
  await mkdir(path.dirname(filePath), { recursive: true });
  await writeFile(filePath, contents, { encoding: "utf8", mode: 0o644 });
}

async function main() {
  const { values } = parseArgs({
    options: {
      version: { type: "string" },
      "git-sha": { type: "string" },
      "source-date-epoch": { type: "string" },
      architecture: { type: "string" },
      "base-image": { type: "string", multiple: true },
      "provider-tools-lockfile": { type: "string" },
      "provider-host-lockfile": { type: "string" },
      "provider-host-package": { type: "string" },
      "worker-apk-lockfile": { type: "string" },
      "raw-provider-tools-sbom": { type: "string" },
      "provider-tools-sbom-output": { type: "string" },
      "manifest-output": { type: "string" },
    },
    strict: true,
  });
  const requiredPaths = [
    "provider-tools-lockfile",
    "provider-host-lockfile",
    "provider-host-package",
    "worker-apk-lockfile",
    "raw-provider-tools-sbom",
    "provider-tools-sbom-output",
    "manifest-output",
  ];
  for (const name of requiredPaths) {
    invariant(typeof values[name] === "string" && values[name].length > 0, `--${name} is required`);
  }
  const artifacts = buildWorkerImageArtifacts({
    version: values.version,
    gitSHA: values["git-sha"],
    sourceDateEpoch: values["source-date-epoch"],
    architecture: values.architecture,
    baseImages: values["base-image"] ?? [],
    providerToolsLockfile: await readFile(values["provider-tools-lockfile"], "utf8"),
    providerHostLockfile: await readFile(values["provider-host-lockfile"], "utf8"),
    providerHostPackageJSON: await readFile(values["provider-host-package"], "utf8"),
    workerAPKLockfile: await readFile(values["worker-apk-lockfile"], "utf8"),
    rawProviderToolsSBOM: await readFile(values["raw-provider-tools-sbom"], "utf8"),
  });
  await writeArtifact(values["provider-tools-sbom-output"], artifacts.providerToolsSBOM);
  await writeArtifact(values["manifest-output"], artifacts.manifestJSON);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 1;
  });
}
