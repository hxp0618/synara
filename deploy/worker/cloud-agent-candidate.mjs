import { createHash } from "node:crypto";

export const CLOUD_AGENT_RELEASE_REPOSITORY = "hxp0618/cloud-agents";
export const CLOUD_AGENT_RELEASE_TAG = "cloud-agent-m1-rc.1";
export const CLOUD_AGENT_RELEASE_BASE_URL =
  "https://github.com/hxp0618/cloud-agents/releases/download/cloud-agent-m1-rc.1/";
export const CLOUD_AGENT_SOURCE_COMMIT = "49e8cdc6a3a4f88c7324d055ce519e9f25a8ca8a";
export const CLOUD_AGENT_STANDALONE_RUNTIME = Object.freeze({
  filename: "cloud-agent-runtime-standalone.mjs",
  sha256: "sha256:c24a1c23f89f62e714e3465b59a4be12e1b5cb75387a2d0d6de9811803064521",
});

export const CLOUD_AGENT_PACKAGE_SPECS = Object.freeze([
  Object.freeze({
    name: "@synara/cloud-agent-distribution",
    version: "0.1.0-rc.1",
    filename: "synara-cloud-agent-distribution-0.1.0-rc.1.tgz",
    sha256: "sha256:0326203b90a5be4f566b3d4282aea5fda8532753855a8a6b4b449776a966e29c",
  }),
  Object.freeze({
    name: "@synara/cloud-agent-protocol",
    version: "0.1.0-rc.1",
    filename: "synara-cloud-agent-protocol-0.1.0-rc.1.tgz",
    sha256: "sha256:e46c5a639482ac8af5c9794cda2c03e93779ce7d10cd1aac52761d6ec3cb2ac2",
  }),
  Object.freeze({
    name: "@synara/cloud-agent-provider-api",
    version: "0.1.0-rc.1",
    filename: "synara-cloud-agent-provider-api-0.1.0-rc.1.tgz",
    sha256: "sha256:88737cda1b0cef0922f50509acb154e2112a2d78acf219a1a5568edf3d731a59",
  }),
  Object.freeze({
    name: "@synara/cloud-agent-provider-claude",
    version: "0.1.0-rc.1",
    filename: "synara-cloud-agent-provider-claude-0.1.0-rc.1.tgz",
    sha256: "sha256:e6a3058fe21fc9799d242bd8c3ae7d8e7bcf7674f2003319d493d4afdda636fd",
  }),
  Object.freeze({
    name: "@synara/cloud-agent-provider-codex",
    version: "0.1.0-rc.1",
    filename: "synara-cloud-agent-provider-codex-0.1.0-rc.1.tgz",
    sha256: "sha256:8715b6c9d116d8b229c810903d1d7da16aaa6fed327020a06e0bd48813f49ec8",
  }),
  Object.freeze({
    name: "@synara/cloud-agent-runtime",
    version: "0.2.0-rc.1",
    filename: "synara-cloud-agent-runtime-0.2.0-rc.1.tgz",
    sha256: "sha256:89aaa8bc42b2527fee584dfa9bccbc3e994dc1c67b99166989ca20be9fcf6488",
  }),
  Object.freeze({
    name: "@synara/cloud-agent-testkit",
    version: "0.1.0-rc.1",
    filename: "synara-cloud-agent-testkit-0.1.0-rc.1.tgz",
    sha256: "sha256:5e4031c8bc449a78e0d5dff6721c7ab708361a5c1b7efa5dfd833752d5add21c",
  }),
]);

export const CLOUD_AGENT_PACKAGE_NAMES = Object.freeze(
  CLOUD_AGENT_PACKAGE_SPECS.map(({ name }) => name),
);

export function canonicalCloudAgentCandidateDigest(packages) {
  const lines = packages
    .map(({ name, version, sha256 }) => `${name}@${version} ${sha256}`)
    .toSorted();
  return sha256(`${lines.join("\n")}\n`);
}

export function validateCloudAgentCandidateLock(value) {
  const lock = parseJSONObject(value, "Cloud Agent candidate lock");
  assertExactKeys(lock, ["schemaVersion", "release", "standaloneRuntime", "packages"], "lock");
  invariant(lock.schemaVersion === 1, "Cloud Agent candidate lock schemaVersion must be 1");

  const release = requireRecord(lock.release, "release");
  assertExactKeys(release, ["repository", "tag", "sourceCommit", "candidateDigest"], "release");
  invariant(
    release.repository === CLOUD_AGENT_RELEASE_REPOSITORY &&
      release.tag === CLOUD_AGENT_RELEASE_TAG &&
      release.sourceCommit === CLOUD_AGENT_SOURCE_COMMIT,
    "Cloud Agent candidate release identity does not match the immutable RC",
  );

  const standalone = requireRecord(lock.standaloneRuntime, "standaloneRuntime");
  assertExactKeys(standalone, ["url", "sha256"], "standaloneRuntime");
  const standaloneURL = `${CLOUD_AGENT_RELEASE_BASE_URL}${CLOUD_AGENT_STANDALONE_RUNTIME.filename}`;
  invariant(
    standalone.url === standaloneURL && standalone.sha256 === CLOUD_AGENT_STANDALONE_RUNTIME.sha256,
    "Cloud Agent standalone Runtime tuple does not match the immutable RC",
  );

  const packages = requireRecord(lock.packages, "packages");
  assertExactKeys(packages, CLOUD_AGENT_PACKAGE_NAMES, "packages");
  const normalizedPackages = CLOUD_AGENT_PACKAGE_SPECS.map((expected) => {
    const artifact = requireRecord(packages[expected.name], expected.name);
    assertExactKeys(artifact, ["version", "url", "sha256"], expected.name);
    const expectedURL = `${CLOUD_AGENT_RELEASE_BASE_URL}${expected.filename}`;
    invariant(
      artifact.version === expected.version &&
        artifact.url === expectedURL &&
        artifact.sha256 === expected.sha256,
      `Cloud Agent candidate package ${expected.name} tuple does not match the immutable RC`,
    );
    return {
      name: expected.name,
      version: expected.version,
      url: expectedURL,
      sha256: expected.sha256,
    };
  });
  const candidateDigest = canonicalCloudAgentCandidateDigest(normalizedPackages);
  invariant(
    release.candidateDigest === candidateDigest,
    `Cloud Agent candidate digest does not match canonical package tuples (${candidateDigest})`,
  );

  return {
    repository: CLOUD_AGENT_RELEASE_REPOSITORY,
    tag: CLOUD_AGENT_RELEASE_TAG,
    sourceCommit: CLOUD_AGENT_SOURCE_COMMIT,
    candidateDigest,
    standaloneRuntime: { url: standaloneURL, sha256: CLOUD_AGENT_STANDALONE_RUNTIME.sha256 },
    packages: normalizedPackages,
    packageVersions: Object.fromEntries(
      normalizedPackages.map(({ name, version }) => [name, version]),
    ),
  };
}

function assertExactKeys(value, expected, label) {
  const actual = Object.keys(value).toSorted();
  const required = [...expected].toSorted();
  invariant(
    actual.length === required.length && actual.every((key, index) => key === required[index]),
    `${label} must contain exactly ${required.join(", ")}`,
  );
}

function parseJSONObject(value, label) {
  let parsed = value;
  if (typeof value === "string") {
    try {
      parsed = JSON.parse(value);
    } catch {
      throw new Error(`${label} must be valid JSON`);
    }
  }
  return requireRecord(parsed, label);
}

function requireRecord(value, label) {
  invariant(
    value !== null && typeof value === "object" && !Array.isArray(value),
    `${label} must be an object`,
  );
  return value;
}

function invariant(condition, message) {
  if (!condition) throw new Error(message);
}

function sha256(value) {
  return `sha256:${createHash("sha256").update(value).digest("hex")}`;
}
