import { createHash } from "node:crypto";
import {
  closeSync,
  constants,
  fstatSync,
  lstatSync,
  openSync,
  readSync,
  readdirSync,
} from "node:fs";
import { basename, join, resolve } from "node:path";

const VERSION_PATTERN = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;
const CHANNEL_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$/;
const FILE_NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._+() -]{0,239}$/;
const SHA512_BASE64_PATTERN = /^[A-Za-z0-9+/]{86}==$/;
const MAX_MANIFEST_BYTES = 4 * 1024 * 1024;
const UNSUPPORTED_UNQUOTED_YAML_CHARACTERS = new Set(":#[]{},&*!|>@`");

export const RELEASE_UPDATER_FEED_SCHEMA = "synara.release-updater-feed-verification.v1";

type UpdatePlatform = "linux" | "mac" | "windows";
type UpdateArchitecture = "arm64" | "x64";

interface ParsedUpdateFile {
  readonly url: string;
  readonly sha512: string;
  readonly size: number;
  readonly blockMapSize?: number;
}

interface ParsedUpdateManifest {
  readonly version: string;
  readonly files: ReadonlyArray<ParsedUpdateFile>;
  readonly path?: string;
  readonly sha512?: string;
}

export interface ReleaseUpdaterPayloadVerification {
  readonly platform: UpdatePlatform;
  readonly arch: UpdateArchitecture;
  readonly fileName: string;
  readonly sizeBytes: number;
  readonly sha512: string;
  readonly blockmapFileName: string | null;
  readonly blockmapSizeBytes: number | null;
}

export interface ReleaseUpdaterManifestVerification {
  readonly platform: UpdatePlatform;
  readonly defaultFileName: string;
  readonly channelFileName: string;
  readonly sha256: string;
}

export interface ReleaseUpdaterFeedVerification {
  readonly schemaVersion: typeof RELEASE_UPDATER_FEED_SCHEMA;
  readonly version: string;
  readonly updateChannel: string;
  readonly manifests: ReadonlyArray<ReleaseUpdaterManifestVerification>;
  readonly payloads: ReadonlyArray<ReleaseUpdaterPayloadVerification>;
  readonly updaterFeedSha256: string;
}

export interface VerifyReleaseUpdaterFeedInput {
  readonly assetsDirectory: string;
  readonly version: string;
  readonly updateChannel: string;
}

interface StableFileHash {
  readonly sizeBytes: number;
  readonly sha256?: string;
  readonly sha512?: string;
}

function validateInput(input: VerifyReleaseUpdaterFeedInput): void {
  if (!VERSION_PATTERN.test(input.version)) throw new Error("Updater release version is invalid.");
  if (!CHANNEL_PATTERN.test(input.updateChannel) || input.updateChannel === "latest") {
    throw new Error("Updater release channel is invalid or reserved.");
  }
}

function validateAssetsDirectory(assetsDirectory: string): string {
  const root = resolve(assetsDirectory);
  const entry = lstatSync(root);
  if (!entry.isDirectory() || entry.isSymbolicLink()) {
    throw new Error("Updater assets directory must be a regular non-symlink directory.");
  }
  return root;
}

function openStableRegularFile(
  path: string,
  label: string,
): {
  readonly descriptor: number;
  readonly size: number;
  readonly mtimeMs: number;
  readonly ctimeMs: number;
} {
  const entry = lstatSync(path);
  if (!entry.isFile() || entry.isSymbolicLink()) {
    throw new Error(`${label} must be a regular non-symlink file.`);
  }
  const descriptor = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  const opened = fstatSync(descriptor);
  if (
    !opened.isFile() ||
    opened.dev !== entry.dev ||
    opened.ino !== entry.ino ||
    opened.size !== entry.size
  ) {
    closeSync(descriptor);
    throw new Error(`${label} changed while it was opened.`);
  }
  return {
    descriptor,
    size: opened.size,
    mtimeMs: opened.mtimeMs,
    ctimeMs: opened.ctimeMs,
  };
}

function verifyStableAfterRead(
  descriptor: number,
  expected: { readonly size: number; readonly mtimeMs: number; readonly ctimeMs: number },
  totalBytes: number,
  label: string,
): void {
  const afterRead = fstatSync(descriptor);
  if (
    totalBytes !== expected.size ||
    afterRead.size !== expected.size ||
    afterRead.mtimeMs !== expected.mtimeMs ||
    afterRead.ctimeMs !== expected.ctimeMs
  ) {
    throw new Error(`${label} changed while it was read.`);
  }
}

function readBoundedRegularFile(path: string, label: string): Buffer {
  const opened = openStableRegularFile(path, label);
  try {
    if (opened.size <= 0 || opened.size > MAX_MANIFEST_BYTES) {
      throw new Error(`${label} has an invalid bounded size.`);
    }
    const bytes = Buffer.allocUnsafe(opened.size);
    let offset = 0;
    while (offset < bytes.byteLength) {
      const bytesRead = readSync(
        opened.descriptor,
        bytes,
        offset,
        bytes.byteLength - offset,
        offset,
      );
      if (bytesRead === 0) throw new Error(`${label} changed while it was read.`);
      offset += bytesRead;
    }
    const trailing = Buffer.allocUnsafe(1);
    if (readSync(opened.descriptor, trailing, 0, 1, offset) !== 0) {
      throw new Error(`${label} changed while it was read.`);
    }
    verifyStableAfterRead(opened.descriptor, opened, offset, label);
    return bytes;
  } finally {
    closeSync(opened.descriptor);
  }
}

function hashRegularFile(
  path: string,
  label: string,
  algorithms: ReadonlyArray<"sha256" | "sha512">,
): StableFileHash {
  const opened = openStableRegularFile(path, label);
  try {
    if (opened.size <= 0) throw new Error(`${label} must not be empty.`);
    const hashes = new Map(algorithms.map((algorithm) => [algorithm, createHash(algorithm)]));
    const buffer = Buffer.allocUnsafe(1024 * 1024);
    let totalBytes = 0;
    for (;;) {
      const bytesRead = readSync(opened.descriptor, buffer, 0, buffer.byteLength, null);
      if (bytesRead === 0) break;
      totalBytes += bytesRead;
      const chunk = buffer.subarray(0, bytesRead);
      for (const hash of hashes.values()) hash.update(chunk);
    }
    verifyStableAfterRead(opened.descriptor, opened, totalBytes, label);
    const sha256Hash = hashes.get("sha256");
    const sha512Hash = hashes.get("sha512");
    return {
      sizeBytes: totalBytes,
      ...(sha256Hash ? { sha256: `sha256:${sha256Hash.digest("hex")}` } : {}),
      ...(sha512Hash ? { sha512: sha512Hash.digest("base64") } : {}),
    };
  } finally {
    closeSync(opened.descriptor);
  }
}

function parseYamlScalar(rawValue: string, label: string): string {
  const value = rawValue.trim();
  if (value.length === 0) throw new Error(`${label} must not be empty.`);
  if (value.startsWith("'") || value.endsWith("'")) {
    if (!(value.startsWith("'") && value.endsWith("'") && value.length >= 2)) {
      throw new Error(`${label} has invalid single-quote syntax.`);
    }
    return value.slice(1, -1).replaceAll("''", "'");
  }
  if (value.startsWith('"') || value.endsWith('"')) {
    try {
      const decoded: unknown = JSON.parse(value);
      if (typeof decoded !== "string") throw new Error("not a string");
      return decoded;
    } catch (error) {
      throw new Error(`${label} has invalid double-quote syntax.`, { cause: error });
    }
  }
  if ([...value].some((character) => UNSUPPORTED_UNQUOTED_YAML_CHARACTERS.has(character))) {
    throw new Error(`${label} contains unsupported unquoted YAML syntax.`);
  }
  return value;
}

function parsePositiveInteger(rawValue: string, label: string): number {
  if (!/^[1-9][0-9]*$/.test(rawValue.trim())) {
    throw new Error(`${label} must be a positive decimal integer.`);
  }
  const value = Number(rawValue.trim());
  if (!Number.isSafeInteger(value)) throw new Error(`${label} exceeds the safe integer range.`);
  return value;
}

function parseUpdateManifest(bytes: Buffer, fileName: string): ParsedUpdateManifest {
  const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  const lines = text.split(/\r?\n/);
  let version: string | undefined;
  let path: string | undefined;
  let topLevelSha512: string | undefined;
  const files: Array<{
    url?: string;
    sha512?: string;
    size?: number;
    blockMapSize?: number;
  }> = [];
  let currentFile: (typeof files)[number] | undefined;
  let sawFiles = false;

  for (const [index, rawLine] of lines.entries()) {
    const lineNumber = index + 1;
    if (rawLine.trim().length === 0) continue;
    if (/\t/.test(rawLine) || rawLine.trimStart().startsWith("#")) {
      throw new Error(`${fileName}:${lineNumber} contains unsupported YAML syntax.`);
    }
    if (rawLine === "files:") {
      if (sawFiles) throw new Error(`${fileName}:${lineNumber} duplicates files.`);
      sawFiles = true;
      currentFile = undefined;
      continue;
    }
    const urlMatch = /^  - url:\s*(.+)$/.exec(rawLine);
    if (urlMatch?.[1]) {
      if (!sawFiles) throw new Error(`${fileName}:${lineNumber} declares a file before files.`);
      currentFile = {
        url: parseYamlScalar(urlMatch[1], `${fileName}:${lineNumber} file URL`),
      };
      files.push(currentFile);
      continue;
    }
    const fileFieldMatch = /^    (sha512|size|blockMapSize):\s*(.+)$/.exec(rawLine);
    if (fileFieldMatch?.[1] && fileFieldMatch[2] !== undefined) {
      if (!currentFile) throw new Error(`${fileName}:${lineNumber} has file metadata without URL.`);
      const key = fileFieldMatch[1] as "sha512" | "size" | "blockMapSize";
      if (currentFile[key] !== undefined) {
        throw new Error(`${fileName}:${lineNumber} duplicates ${key}.`);
      }
      if (key === "sha512") {
        currentFile.sha512 = parseYamlScalar(fileFieldMatch[2], `${fileName}:${lineNumber} sha512`);
      } else {
        currentFile[key] = parsePositiveInteger(
          fileFieldMatch[2],
          `${fileName}:${lineNumber} ${key}`,
        );
      }
      continue;
    }
    const topLevelMatch = /^([A-Za-z][A-Za-z0-9]*):\s*(.*)$/.exec(rawLine);
    if (!topLevelMatch?.[1] || topLevelMatch[2] === undefined) {
      throw new Error(`${fileName}:${lineNumber} contains unsupported YAML structure.`);
    }
    currentFile = undefined;
    const key = topLevelMatch[1];
    const rawValue = topLevelMatch[2];
    if (key === "version") {
      if (version !== undefined) throw new Error(`${fileName}:${lineNumber} duplicates version.`);
      version = parseYamlScalar(rawValue, `${fileName}:${lineNumber} version`);
    } else if (key === "path") {
      if (path !== undefined) throw new Error(`${fileName}:${lineNumber} duplicates path.`);
      path = parseYamlScalar(rawValue, `${fileName}:${lineNumber} path`);
    } else if (key === "sha512") {
      if (topLevelSha512 !== undefined) {
        throw new Error(`${fileName}:${lineNumber} duplicates top-level sha512.`);
      }
      topLevelSha512 = parseYamlScalar(rawValue, `${fileName}:${lineNumber} sha512`);
    } else {
      // electron-builder may add scalar release metadata. Parse it strictly so
      // nested YAML, aliases, tags, and multiline values cannot affect updater semantics.
      parseYamlScalar(rawValue, `${fileName}:${lineNumber} ${key}`);
    }
  }

  if (!version || !sawFiles || files.length === 0) {
    throw new Error(`${fileName} must contain version and a non-empty files list.`);
  }
  const normalizedFiles = files.map((file, index): ParsedUpdateFile => {
    if (
      file.url === undefined ||
      file.sha512 === undefined ||
      file.size === undefined ||
      !SHA512_BASE64_PATTERN.test(file.sha512)
    ) {
      throw new Error(`${fileName} file entry ${index} is incomplete or has an invalid SHA-512.`);
    }
    return file.blockMapSize === undefined
      ? { url: file.url, sha512: file.sha512, size: file.size }
      : {
          url: file.url,
          sha512: file.sha512,
          size: file.size,
          blockMapSize: file.blockMapSize,
        };
  });
  if ((path === undefined) !== (topLevelSha512 === undefined)) {
    throw new Error(`${fileName} must provide top-level path and sha512 together.`);
  }
  if (topLevelSha512 !== undefined && !SHA512_BASE64_PATTERN.test(topLevelSha512)) {
    throw new Error(`${fileName} top-level sha512 is invalid.`);
  }
  if (path !== undefined) {
    const selected = normalizedFiles.filter((file) => file.url === path);
    if (selected.length !== 1 || selected[0]!.sha512 !== topLevelSha512) {
      throw new Error(`${fileName} top-level path and sha512 do not match exactly one file entry.`);
    }
  }
  return {
    version,
    files: normalizedFiles,
    ...(path === undefined ? {} : { path }),
    ...(topLevelSha512 === undefined ? {} : { sha512: topLevelSha512 }),
  };
}

function validatePayloadFileName(fileName: string, label: string): string {
  if (
    basename(fileName) !== fileName ||
    fileName !== fileName.normalize("NFC") ||
    !FILE_NAME_PATTERN.test(fileName) ||
    fileName.includes("..") ||
    /[%?#\\/]/.test(fileName) ||
    /^[A-Za-z][A-Za-z0-9+.-]*:/.test(fileName)
  ) {
    throw new Error(`${label} must be a safe public asset basename, not a path or external URL.`);
  }
  return fileName;
}

function requireArchitecture(fileName: string, architecture: UpdateArchitecture): void {
  const token = new RegExp(`(?:^|[._ +()-])${architecture}(?:$|[._ +()-])`);
  if (!token.test(fileName.slice(0, fileName.lastIndexOf(".")))) {
    throw new Error(`macOS updater payload ${fileName} does not identify ${architecture}.`);
  }
}

function verifyPayload(
  root: string,
  platform: UpdatePlatform,
  arch: UpdateArchitecture,
  file: ParsedUpdateFile,
  availableFiles: ReadonlySet<string>,
  requireBlockmap: boolean,
): ReleaseUpdaterPayloadVerification {
  const fileName = validatePayloadFileName(file.url, `${platform}/${arch} updater URL`);
  if (!availableFiles.has(fileName)) {
    throw new Error(
      `${platform}/${arch} updater payload is missing from final assets: ${fileName}.`,
    );
  }
  const payload = hashRegularFile(join(root, fileName), `${platform}/${arch} payload ${fileName}`, [
    "sha512",
  ]);
  if (payload.sizeBytes !== file.size) {
    throw new Error(`${platform}/${arch} updater payload size does not match ${fileName}.`);
  }
  if (payload.sha512 !== file.sha512) {
    throw new Error(`${platform}/${arch} updater SHA-512 does not match ${fileName}.`);
  }

  const blockmapFileName = `${fileName}.blockmap`;
  const hasBlockmap = availableFiles.has(blockmapFileName);
  if (requireBlockmap && !hasBlockmap) {
    throw new Error(`${platform}/${arch} updater requires blockmap ${blockmapFileName}.`);
  }
  if (file.blockMapSize !== undefined && !hasBlockmap) {
    throw new Error(`${platform}/${arch} updater declares a missing blockmap for ${fileName}.`);
  }
  let blockmapSizeBytes: number | null = null;
  if (hasBlockmap) {
    const blockmap = hashRegularFile(
      join(root, blockmapFileName),
      `${platform}/${arch} blockmap ${blockmapFileName}`,
      [],
    );
    blockmapSizeBytes = blockmap.sizeBytes;
    if (file.blockMapSize !== undefined && file.blockMapSize !== blockmap.sizeBytes) {
      throw new Error(
        `${platform}/${arch} updater blockMapSize does not match ${blockmapFileName}.`,
      );
    }
  }
  return Object.freeze({
    platform,
    arch,
    fileName,
    sizeBytes: payload.sizeBytes,
    sha512: file.sha512,
    blockmapFileName: hasBlockmap ? blockmapFileName : null,
    blockmapSizeBytes,
  });
}

function manifestNames(updateChannel: string): ReadonlyArray<{
  readonly platform: UpdatePlatform;
  readonly defaultFileName: string;
  readonly channelFileName: string;
}> {
  return [
    {
      platform: "mac",
      defaultFileName: "latest-mac.yml",
      channelFileName: `${updateChannel}-mac.yml`,
    },
    {
      platform: "windows",
      defaultFileName: "latest.yml",
      channelFileName: `${updateChannel}.yml`,
    },
    {
      platform: "linux",
      defaultFileName: "latest-linux.yml",
      channelFileName: `${updateChannel}-linux.yml`,
    },
  ];
}

export function verifyReleaseUpdaterFeed(
  input: VerifyReleaseUpdaterFeedInput,
): ReleaseUpdaterFeedVerification {
  validateInput(input);
  const root = validateAssetsDirectory(input.assetsDirectory);
  const availableFiles = new Set(readdirSync(root));
  const parsed = new Map<UpdatePlatform, ParsedUpdateManifest>();
  const manifests: ReleaseUpdaterManifestVerification[] = [];

  for (const names of manifestNames(input.updateChannel)) {
    const defaultBytes = readBoundedRegularFile(
      join(root, names.defaultFileName),
      `${names.platform} default updater manifest`,
    );
    const channelBytes = readBoundedRegularFile(
      join(root, names.channelFileName),
      `${names.platform} channel updater manifest`,
    );
    if (!defaultBytes.equals(channelBytes)) {
      throw new Error(
        `${names.platform} latest and ${input.updateChannel} updater manifests must be byte-identical aliases.`,
      );
    }
    const manifest = parseUpdateManifest(defaultBytes, names.defaultFileName);
    if (manifest.version !== input.version) {
      throw new Error(
        `${names.defaultFileName} version ${manifest.version} does not match release ${input.version}.`,
      );
    }
    parsed.set(names.platform, manifest);
    manifests.push(
      Object.freeze({
        ...names,
        sha256: `sha256:${createHash("sha256").update(defaultBytes).digest("hex")}`,
      }),
    );
  }

  const mac = parsed.get("mac")!;
  if (mac.files.length !== 2 || mac.files.some((file) => !file.url.endsWith(".zip"))) {
    throw new Error("macOS updater manifest must contain exactly the arm64 and x64 ZIP payloads.");
  }
  const macArm64 = mac.files.filter((file) => {
    try {
      requireArchitecture(file.url, "arm64");
      return true;
    } catch {
      return false;
    }
  });
  const macX64 = mac.files.filter((file) => {
    try {
      requireArchitecture(file.url, "x64");
      return true;
    } catch {
      return false;
    }
  });
  if (macArm64.length !== 1 || macX64.length !== 1 || macArm64[0] === macX64[0]) {
    throw new Error("macOS updater manifest must identify exactly one arm64 and one x64 ZIP.");
  }

  const windows = parsed.get("windows")!;
  if (windows.files.length !== 1 || !windows.files[0]!.url.endsWith(".exe")) {
    throw new Error("Windows updater manifest must contain exactly one EXE payload.");
  }
  const linux = parsed.get("linux")!;
  if (linux.files.length !== 1 || !linux.files[0]!.url.endsWith(".AppImage")) {
    throw new Error("Linux updater manifest must contain exactly one AppImage payload.");
  }

  const payloads = [
    verifyPayload(root, "mac", "arm64", macArm64[0]!, availableFiles, false),
    verifyPayload(root, "mac", "x64", macX64[0]!, availableFiles, false),
    verifyPayload(root, "windows", "x64", windows.files[0]!, availableFiles, true),
    verifyPayload(root, "linux", "x64", linux.files[0]!, availableFiles, false),
  ].toSorted((left, right) =>
    `${left.platform}/${left.arch}`.localeCompare(`${right.platform}/${right.arch}`),
  );

  const expectedPayloadNames = new Set(payloads.map((payload) => payload.fileName));
  const actualPayloadNames = [...availableFiles].filter((fileName) =>
    /\.(?:zip|exe|AppImage)$/.test(fileName),
  );
  if (
    actualPayloadNames.length !== expectedPayloadNames.size ||
    actualPayloadNames.some((fileName) => !expectedPayloadNames.has(fileName))
  ) {
    throw new Error("Final updater payload files do not exactly match the updater manifests.");
  }
  const expectedBlockmaps = new Set(
    payloads
      .map((payload) => payload.blockmapFileName)
      .filter((fileName): fileName is string => fileName !== null),
  );
  const actualBlockmaps = [...availableFiles].filter((fileName) => fileName.endsWith(".blockmap"));
  if (
    actualBlockmaps.length !== expectedBlockmaps.size ||
    actualBlockmaps.some((fileName) => !expectedBlockmaps.has(fileName))
  ) {
    throw new Error("Final updater blockmaps do not exactly match their updater payloads.");
  }

  const canonical = {
    schemaVersion: RELEASE_UPDATER_FEED_SCHEMA,
    version: input.version,
    updateChannel: input.updateChannel,
    manifests,
    payloads,
  } satisfies Omit<ReleaseUpdaterFeedVerification, "updaterFeedSha256">;
  const updaterFeedSha256 = `sha256:${createHash("sha256")
    .update(JSON.stringify(canonical))
    .digest("hex")}`;
  return Object.freeze({
    ...canonical,
    manifests: Object.freeze(manifests),
    payloads: Object.freeze(payloads),
    updaterFeedSha256,
  });
}
