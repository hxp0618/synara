import { createHash } from "node:crypto";
import {
  closeSync,
  constants,
  fstatSync,
  lstatSync,
  openSync,
  readSync,
  readdirSync,
  writeFileSync,
} from "node:fs";
import { basename, join, resolve } from "node:path";
import { TextDecoder } from "node:util";

import { RELEASE_UPDATER_FEED_SCHEMA, verifyReleaseUpdaterFeed } from "./release-updater-feed.ts";

const COMMIT_PATTERN = /^[0-9a-f]{40}$/;
const HEX_SHA256_PATTERN = /^[0-9a-f]{64}$/;
const SHA256_PATTERN = /^sha256:[0-9a-f]{64}$/;
const VERSION_PATTERN = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;
const CHANNEL_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$/;
const FILE_NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._+() -]{0,239}$/;
const MAX_MANIFEST_BYTES = 1024 * 1024;
const MAX_PUBLIC_FILES = 256;
const ZERO_SHA256 = "0".repeat(64);

export const RELEASE_FINAL_ASSET_SET_SCHEMA = "synara.release-final-asset-set.v1";
export const RELEASE_FINAL_ASSET_SET_ASSESSMENT = "final-assets-bound-not-published";
export const RELEASE_FINAL_ASSET_SET_MANIFEST = "release-final-asset-set.json";
export const RELEASE_FINAL_ASSET_SET_SIDECAR = `${RELEASE_FINAL_ASSET_SET_MANIFEST}.sha256`;

const DEFAULT_UPDATE_MANIFESTS = ["latest-mac.yml", "latest.yml", "latest-linux.yml"] as const;
const TRUST_TARGETS = ["linux-x64", "mac-arm64", "mac-x64", "win-x64"] as const;
const TRUST_SUFFIXES = [
  "provenance.json",
  "attestation.json",
  "attestation-verification.json",
] as const;
const REQUIRED_TRUST_FILES = TRUST_TARGETS.flatMap((target) =>
  TRUST_SUFFIXES.map((suffix) => `artifact-${target}.${suffix}`),
).toSorted();
const REQUIRED_TRUST_FILE_SET = new Set(REQUIRED_TRUST_FILES);

const VERIFIED_RELEASE_FINAL_ASSET_SET: unique symbol = Symbol("VerifiedReleaseFinalAssetSet");
const VERIFIED_RELEASE_FINAL_ASSET_SETS = new WeakSet<object>();

export interface ReleaseFinalAssetSetIdentity {
  readonly sourceTag: string;
  readonly sourceCommit: string;
  readonly lockfileSha256: string;
  readonly version: string;
  readonly updateChannel: string;
}

export interface ReleaseFinalAssetSetInput extends ReleaseFinalAssetSetIdentity {
  readonly assetsDirectory: string;
}

export interface VerifyReleaseFinalAssetSetInput extends ReleaseFinalAssetSetInput {
  readonly expectedFinalAssetSetSha256?: string;
}

export interface ReleaseFinalAssetFile {
  readonly fileName: string;
  readonly sizeBytes: number;
  readonly sha256: string;
}

export interface ReleaseFinalAssetSetManifest {
  readonly schemaVersion: typeof RELEASE_FINAL_ASSET_SET_SCHEMA;
  readonly source: {
    readonly tag: string;
    readonly commit: string;
    readonly lockfileSha256: string;
  };
  readonly version: string;
  readonly updateChannel: string;
  readonly updaterFeed: {
    readonly schemaVersion: typeof RELEASE_UPDATER_FEED_SCHEMA;
    readonly sha256: string;
  };
  readonly files: ReadonlyArray<ReleaseFinalAssetFile>;
  readonly finalAssetSetSha256: string;
  readonly assessment: typeof RELEASE_FINAL_ASSET_SET_ASSESSMENT;
}

export interface ReleaseFinalAssetSetWriteResult {
  readonly manifest: ReleaseFinalAssetSetManifest;
  readonly manifestPath: string;
  readonly sidecarPath: string;
}

export interface VerifiedReleaseFinalAssetSet extends ReleaseFinalAssetSetManifest {
  readonly [VERIFIED_RELEASE_FINAL_ASSET_SET]: true;
}

export function assertVerifiedReleaseFinalAssetSet(
  value: unknown,
): asserts value is VerifiedReleaseFinalAssetSet {
  if (
    typeof value !== "object" ||
    value === null ||
    !VERIFIED_RELEASE_FINAL_ASSET_SETS.has(value)
  ) {
    throw new Error("Release final asset set was not produced by the local byte verifier.");
  }
}

function requireObject(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object.`);
  }
  return value as Record<string, unknown>;
}

function requireExactFields(
  value: unknown,
  fields: ReadonlyArray<string>,
  label: string,
): Record<string, unknown> {
  const object = requireObject(value, label);
  if (JSON.stringify(Object.keys(object).toSorted()) !== JSON.stringify([...fields].toSorted())) {
    throw new Error(`${label} fields do not match the final asset-set schema.`);
  }
  return object;
}

function validateIdentity(identity: ReleaseFinalAssetSetIdentity): ReleaseFinalAssetSetIdentity {
  if (!VERSION_PATTERN.test(identity.version)) throw new Error("Release version is invalid.");
  if (identity.sourceTag !== `v${identity.version}`) {
    throw new Error("Release source tag must exactly match the release version.");
  }
  if (!COMMIT_PATTERN.test(identity.sourceCommit)) {
    throw new Error("Release source commit must be 40 lowercase hex characters.");
  }
  if (
    !HEX_SHA256_PATTERN.test(identity.lockfileSha256) ||
    identity.lockfileSha256 === ZERO_SHA256
  ) {
    throw new Error("Release lockfile SHA-256 must be a non-zero lowercase digest.");
  }
  if (!CHANNEL_PATTERN.test(identity.updateChannel) || identity.updateChannel === "latest") {
    throw new Error("Release update channel is invalid or reserved.");
  }
  return identity;
}

function validateAssetsDirectory(assetsDirectory: string): string {
  const root = resolve(assetsDirectory);
  const entry = lstatSync(root);
  if (!entry.isDirectory() || entry.isSymbolicLink()) {
    throw new Error("Release assets directory must be a regular non-symlink directory.");
  }
  return root;
}

function hashRegularFile(path: string, label: string): { sizeBytes: number; sha256: string } {
  const entry = lstatSync(path);
  if (!entry.isFile() || entry.isSymbolicLink()) {
    throw new Error(`${label} must be a regular non-symlink file.`);
  }
  if (entry.size <= 0) throw new Error(`${label} must not be empty.`);

  const descriptor = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const opened = fstatSync(descriptor);
    if (!opened.isFile() || opened.dev !== entry.dev || opened.ino !== entry.ino) {
      throw new Error(`${label} changed while it was opened.`);
    }
    const hash = createHash("sha256");
    const buffer = Buffer.allocUnsafe(1024 * 1024);
    let total = 0;
    for (;;) {
      const bytesRead = readSync(descriptor, buffer, 0, buffer.byteLength, null);
      if (bytesRead === 0) break;
      total += bytesRead;
      hash.update(buffer.subarray(0, bytesRead));
    }
    const afterRead = fstatSync(descriptor);
    if (
      total !== opened.size ||
      afterRead.size !== opened.size ||
      afterRead.mtimeMs !== opened.mtimeMs ||
      afterRead.ctimeMs !== opened.ctimeMs
    ) {
      throw new Error(`${label} changed while it was hashed.`);
    }
    return { sizeBytes: total, sha256: `sha256:${hash.digest("hex")}` };
  } finally {
    closeSync(descriptor);
  }
}

function readBoundedRegularFile(path: string, label: string): Buffer {
  const entry = lstatSync(path);
  if (!entry.isFile() || entry.isSymbolicLink()) {
    throw new Error(`${label} must be a regular non-symlink file.`);
  }
  if (entry.size <= 0 || entry.size > MAX_MANIFEST_BYTES) {
    throw new Error(`${label} has an invalid bounded size.`);
  }
  const descriptor = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const opened = fstatSync(descriptor);
    if (
      !opened.isFile() ||
      opened.dev !== entry.dev ||
      opened.ino !== entry.ino ||
      opened.size !== entry.size
    ) {
      throw new Error(`${label} changed while it was opened.`);
    }
    const bytes = Buffer.allocUnsafe(opened.size);
    let offset = 0;
    while (offset < bytes.byteLength) {
      const bytesRead = readSync(descriptor, bytes, offset, bytes.byteLength - offset, offset);
      if (bytesRead === 0) throw new Error(`${label} changed while it was read.`);
      offset += bytesRead;
    }
    const trailingByte = Buffer.allocUnsafe(1);
    if (readSync(descriptor, trailingByte, 0, 1, offset) !== 0) {
      throw new Error(`${label} changed while it was read.`);
    }
    const afterRead = fstatSync(descriptor);
    if (
      afterRead.size !== opened.size ||
      afterRead.mtimeMs !== opened.mtimeMs ||
      afterRead.ctimeMs !== opened.ctimeMs
    ) {
      throw new Error(`${label} changed while it was read.`);
    }
    return bytes;
  } finally {
    closeSync(descriptor);
  }
}

function channelManifestNames(channel: string): readonly string[] {
  return [`${channel}-mac.yml`, `${channel}.yml`, `${channel}-linux.yml`];
}

function publicFileKind(fileName: string): "payload" | "update" | "trust" | null {
  if (REQUIRED_TRUST_FILE_SET.has(fileName)) return "trust";
  if (fileName.endsWith(".yml")) return "update";
  if (/\.(?:dmg|zip|AppImage|exe|blockmap)$/.test(fileName)) return "payload";
  return null;
}

function validateFileNames(fileNames: ReadonlyArray<string>, updateChannel: string): void {
  if (fileNames.length > MAX_PUBLIC_FILES) {
    throw new Error(`Release asset set exceeds ${MAX_PUBLIC_FILES} public files.`);
  }
  const canonicalNames = new Set<string>();
  for (const fileName of fileNames) {
    if (
      basename(fileName) !== fileName ||
      fileName !== fileName.normalize("NFC") ||
      !FILE_NAME_PATTERN.test(fileName)
    ) {
      throw new Error(`Illegal release asset file name: ${fileName}.`);
    }
    const canonicalName = fileName.toLowerCase();
    if (canonicalNames.has(canonicalName)) {
      throw new Error(`Duplicate release asset file name: ${fileName}.`);
    }
    canonicalNames.add(canonicalName);
  }

  if (fileNames.includes("latest-mac-x64.yml")) {
    throw new Error("Stale latest-mac-x64.yml must be removed before final asset binding.");
  }
  const expectedYml = [
    ...DEFAULT_UPDATE_MANIFESTS,
    ...channelManifestNames(updateChannel),
  ].toSorted();
  const actualYml = fileNames.filter((name) => name.endsWith(".yml")).toSorted();
  if (JSON.stringify(actualYml) !== JSON.stringify(expectedYml)) {
    throw new Error(
      "Release asset set must contain exactly the default and dedicated-channel updater manifests.",
    );
  }
  const actualTrust = fileNames.filter((name) => REQUIRED_TRUST_FILE_SET.has(name)).toSorted();
  if (JSON.stringify(actualTrust) !== JSON.stringify(REQUIRED_TRUST_FILES)) {
    throw new Error("Release asset set must contain all four targets' exact trust evidence files.");
  }
  for (const fileName of fileNames) {
    if (publicFileKind(fileName) === null) {
      throw new Error(`Non-public release asset is not allowed: ${fileName}.`);
    }
  }

  const count = (suffix: string): number =>
    fileNames.filter((name) => name.endsWith(suffix)).length;
  if (count(".dmg") < 2 || count(".zip") < 2 || count(".AppImage") < 1 || count(".exe") < 1) {
    throw new Error("Release asset set is missing the minimum cross-platform installer payloads.");
  }
}

function collectFinalAssets(
  root: string,
  updateChannel: string,
): ReadonlyArray<ReleaseFinalAssetFile> {
  const entries = readdirSync(root);
  if (
    entries.includes(RELEASE_FINAL_ASSET_SET_MANIFEST) ||
    entries.includes(RELEASE_FINAL_ASSET_SET_SIDECAR)
  ) {
    throw new Error("Release final asset-set manifest already exists and will not be overwritten.");
  }
  validateFileNames(entries, updateChannel);
  return entries.toSorted().map((fileName) => {
    const digest = hashRegularFile(join(root, fileName), `Release asset ${fileName}`);
    return Object.freeze({
      fileName,
      sizeBytes: digest.sizeBytes,
      sha256: digest.sha256,
    });
  });
}

function canonicalDigestInput(
  identity: ReleaseFinalAssetSetIdentity,
  files: ReadonlyArray<ReleaseFinalAssetFile>,
  updaterFeedSha256: string,
): string {
  return JSON.stringify({
    schemaVersion: RELEASE_FINAL_ASSET_SET_SCHEMA,
    source: {
      tag: identity.sourceTag,
      commit: identity.sourceCommit,
      lockfileSha256: identity.lockfileSha256,
    },
    version: identity.version,
    updateChannel: identity.updateChannel,
    updaterFeed: {
      schemaVersion: RELEASE_UPDATER_FEED_SCHEMA,
      sha256: updaterFeedSha256,
    },
    files,
    assessment: RELEASE_FINAL_ASSET_SET_ASSESSMENT,
  });
}

export function computeReleaseFinalAssetSetSha256(
  identity: ReleaseFinalAssetSetIdentity,
  files: ReadonlyArray<ReleaseFinalAssetFile>,
  updaterFeedSha256: string,
): string {
  validateIdentity(identity);
  if (!SHA256_PATTERN.test(updaterFeedSha256)) {
    throw new Error("Release updater-feed SHA-256 is invalid.");
  }
  return `sha256:${createHash("sha256")
    .update(canonicalDigestInput(identity, files, updaterFeedSha256))
    .digest("hex")}`;
}

function createManifest(
  identity: ReleaseFinalAssetSetIdentity,
  files: ReadonlyArray<ReleaseFinalAssetFile>,
  updaterFeedSha256: string,
): ReleaseFinalAssetSetManifest {
  return {
    schemaVersion: RELEASE_FINAL_ASSET_SET_SCHEMA,
    source: {
      tag: identity.sourceTag,
      commit: identity.sourceCommit,
      lockfileSha256: identity.lockfileSha256,
    },
    version: identity.version,
    updateChannel: identity.updateChannel,
    updaterFeed: {
      schemaVersion: RELEASE_UPDATER_FEED_SCHEMA,
      sha256: updaterFeedSha256,
    },
    files,
    finalAssetSetSha256: computeReleaseFinalAssetSetSha256(identity, files, updaterFeedSha256),
    assessment: RELEASE_FINAL_ASSET_SET_ASSESSMENT,
  };
}

function encodeManifest(manifest: ReleaseFinalAssetSetManifest): string {
  return `${JSON.stringify(manifest, null, 2)}\n`;
}

export function writeReleaseFinalAssetSet(
  input: ReleaseFinalAssetSetInput,
): ReleaseFinalAssetSetWriteResult {
  const identity = validateIdentity(input);
  const root = validateAssetsDirectory(input.assetsDirectory);
  const updaterFeed = verifyReleaseUpdaterFeed({
    assetsDirectory: root,
    version: identity.version,
    updateChannel: identity.updateChannel,
  });
  const files = collectFinalAssets(root, identity.updateChannel);
  const manifest = createManifest(identity, files, updaterFeed.updaterFeedSha256);
  const manifestBytes = encodeManifest(manifest);
  const manifestSha256 = createHash("sha256").update(manifestBytes).digest("hex");
  const sidecarBytes = `${manifestSha256}  ${RELEASE_FINAL_ASSET_SET_MANIFEST}\n`;
  const manifestPath = join(root, RELEASE_FINAL_ASSET_SET_MANIFEST);
  const sidecarPath = join(root, RELEASE_FINAL_ASSET_SET_SIDECAR);

  const manifestDescriptor = openSync(manifestPath, "wx");
  let sidecarDescriptor: number | undefined;
  try {
    sidecarDescriptor = openSync(sidecarPath, "wx");
    writeFileSync(manifestDescriptor, manifestBytes);
    writeFileSync(sidecarDescriptor, sidecarBytes);
  } finally {
    if (sidecarDescriptor !== undefined) closeSync(sidecarDescriptor);
    closeSync(manifestDescriptor);
  }
  return { manifest, manifestPath, sidecarPath };
}

function parseManifest(bytes: Buffer): ReleaseFinalAssetSetManifest {
  let decoded: unknown;
  try {
    decoded = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
  } catch (error) {
    throw new Error("Release final asset-set manifest must be valid UTF-8 JSON.", { cause: error });
  }
  const object = requireExactFields(
    decoded,
    [
      "schemaVersion",
      "source",
      "version",
      "updateChannel",
      "updaterFeed",
      "files",
      "finalAssetSetSha256",
      "assessment",
    ],
    "Release final asset-set manifest",
  );
  const source = requireExactFields(
    object.source,
    ["tag", "commit", "lockfileSha256"],
    "Release final asset-set source",
  );
  const updaterFeed = requireExactFields(
    object.updaterFeed,
    ["schemaVersion", "sha256"],
    "Release final asset-set updater feed",
  );
  if (!Array.isArray(object.files))
    throw new Error("Release final asset-set files must be an array.");
  const files = object.files.map((rawFile, index): ReleaseFinalAssetFile => {
    const file = requireExactFields(
      rawFile,
      ["fileName", "sizeBytes", "sha256"],
      `Release final asset ${index}`,
    );
    if (
      typeof file.fileName !== "string" ||
      !Number.isSafeInteger(file.sizeBytes) ||
      (file.sizeBytes as number) <= 0 ||
      typeof file.sha256 !== "string" ||
      !SHA256_PATTERN.test(file.sha256)
    ) {
      throw new Error(`Release final asset ${index} metadata is invalid.`);
    }
    return { fileName: file.fileName, sizeBytes: file.sizeBytes as number, sha256: file.sha256 };
  });
  if (
    object.schemaVersion !== RELEASE_FINAL_ASSET_SET_SCHEMA ||
    object.assessment !== RELEASE_FINAL_ASSET_SET_ASSESSMENT ||
    typeof source.tag !== "string" ||
    typeof source.commit !== "string" ||
    typeof source.lockfileSha256 !== "string" ||
    typeof object.version !== "string" ||
    typeof object.updateChannel !== "string" ||
    updaterFeed.schemaVersion !== RELEASE_UPDATER_FEED_SCHEMA ||
    typeof updaterFeed.sha256 !== "string" ||
    !SHA256_PATTERN.test(updaterFeed.sha256) ||
    typeof object.finalAssetSetSha256 !== "string" ||
    !SHA256_PATTERN.test(object.finalAssetSetSha256)
  ) {
    throw new Error("Release final asset-set manifest identity or assessment is invalid.");
  }
  return {
    schemaVersion: RELEASE_FINAL_ASSET_SET_SCHEMA,
    source: { tag: source.tag, commit: source.commit, lockfileSha256: source.lockfileSha256 },
    version: object.version,
    updateChannel: object.updateChannel,
    updaterFeed: {
      schemaVersion: RELEASE_UPDATER_FEED_SCHEMA,
      sha256: updaterFeed.sha256,
    },
    files,
    finalAssetSetSha256: object.finalAssetSetSha256,
    assessment: RELEASE_FINAL_ASSET_SET_ASSESSMENT,
  };
}

function compareFiles(
  declared: ReadonlyArray<ReleaseFinalAssetFile>,
  actual: ReadonlyArray<ReleaseFinalAssetFile>,
): void {
  if (JSON.stringify(declared) !== JSON.stringify(actual)) {
    throw new Error("Release final asset bytes do not match the bound manifest.");
  }
}

export function verifyReleaseFinalAssetSet(
  input: VerifyReleaseFinalAssetSetInput,
): VerifiedReleaseFinalAssetSet {
  const expectedIdentity = validateIdentity(input);
  if (
    input.expectedFinalAssetSetSha256 !== undefined &&
    !SHA256_PATTERN.test(input.expectedFinalAssetSetSha256)
  ) {
    throw new Error("Expected release final asset-set SHA-256 is invalid.");
  }
  const root = validateAssetsDirectory(input.assetsDirectory);
  const manifestPath = join(root, RELEASE_FINAL_ASSET_SET_MANIFEST);
  const sidecarPath = join(root, RELEASE_FINAL_ASSET_SET_SIDECAR);
  const manifestBytes = readBoundedRegularFile(manifestPath, "Release final asset-set manifest");
  const sidecarBytes = readBoundedRegularFile(sidecarPath, "Release final asset-set sidecar");
  const manifestSha256 = createHash("sha256").update(manifestBytes).digest("hex");
  const expectedSidecar = `${manifestSha256}  ${RELEASE_FINAL_ASSET_SET_MANIFEST}\n`;
  if (!sidecarBytes.equals(Buffer.from(expectedSidecar))) {
    throw new Error("Release final asset-set sidecar does not match the manifest bytes.");
  }

  const manifest = parseManifest(manifestBytes);
  if (
    manifest.source.tag !== expectedIdentity.sourceTag ||
    manifest.source.commit !== expectedIdentity.sourceCommit ||
    manifest.source.lockfileSha256 !== expectedIdentity.lockfileSha256 ||
    manifest.version !== expectedIdentity.version ||
    manifest.updateChannel !== expectedIdentity.updateChannel
  ) {
    throw new Error("Release final asset-set identity does not match the expected release source.");
  }

  const updaterFeed = verifyReleaseUpdaterFeed({
    assetsDirectory: root,
    version: expectedIdentity.version,
    updateChannel: expectedIdentity.updateChannel,
  });
  if (manifest.updaterFeed.sha256 !== updaterFeed.updaterFeedSha256) {
    throw new Error("Release final asset-set updater feed does not match its semantic summary.");
  }

  const publicNames = readdirSync(root)
    .filter(
      (name) =>
        name !== RELEASE_FINAL_ASSET_SET_MANIFEST && name !== RELEASE_FINAL_ASSET_SET_SIDECAR,
    )
    .toSorted();
  validateFileNames(publicNames, expectedIdentity.updateChannel);
  const actualFiles = publicNames.map((fileName) => {
    const digest = hashRegularFile(join(root, fileName), `Release asset ${fileName}`);
    return { fileName, sizeBytes: digest.sizeBytes, sha256: digest.sha256 };
  });
  compareFiles(manifest.files, actualFiles);
  const recomputed = computeReleaseFinalAssetSetSha256(
    expectedIdentity,
    actualFiles,
    updaterFeed.updaterFeedSha256,
  );
  if (manifest.finalAssetSetSha256 !== recomputed) {
    throw new Error("Release final asset-set digest does not match its canonical content.");
  }
  if (
    input.expectedFinalAssetSetSha256 !== undefined &&
    input.expectedFinalAssetSetSha256 !== recomputed
  ) {
    throw new Error("Release final asset-set digest does not match the expected approved digest.");
  }
  if (!manifestBytes.equals(Buffer.from(encodeManifest(manifest)))) {
    throw new Error("Release final asset-set manifest is not canonical JSON.");
  }

  const value = {
    ...manifest,
    source: Object.freeze({ ...manifest.source }),
    updaterFeed: Object.freeze({ ...manifest.updaterFeed }),
    files: Object.freeze(manifest.files.map((file) => Object.freeze({ ...file }))),
  } as ReleaseFinalAssetSetManifest;
  Object.defineProperty(value, VERIFIED_RELEASE_FINAL_ASSET_SET, {
    value: true,
    enumerable: false,
    writable: false,
    configurable: false,
  });
  const verified = Object.freeze(value) as VerifiedReleaseFinalAssetSet;
  VERIFIED_RELEASE_FINAL_ASSET_SETS.add(verified);
  return verified;
}
