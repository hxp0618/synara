import { createHash } from "node:crypto";
import { closeSync, lstatSync, openSync, readFileSync, readSync, readdirSync } from "node:fs";
import { basename, join, resolve } from "node:path";

const COMMIT_PATTERN = /^[0-9a-f]{40}$/;
const HEX_SHA256_PATTERN = /^[0-9a-f]{64}$/;
const SHA256_PATTERN = /^sha256:[0-9a-f]{64}$/;
const FILE_NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._+() -]{0,239}$/;
const MAX_PROVENANCE_BYTES = 16 * 1024 * 1024;
const MAX_ATTESTATION_BYTES = 32 * 1024 * 1024;
const ARTIFACT_SET_EVIDENCE_FIELDS = [
  "artifactAttestation",
  "attestationBundle",
  "provenance",
] as const;
type Stage6DesktopArtifactSetEvidenceField = (typeof ARTIFACT_SET_EVIDENCE_FIELDS)[number];

const TARGETS = {
  "linux-x64": {
    platform: "linux",
    arch: "x64",
    target: "AppImage",
    primarySuffix: ".AppImage",
    provenanceFile: "artifact-linux-x64.provenance.json",
  },
  "macos-arm64": {
    platform: "mac",
    arch: "arm64",
    target: "dmg",
    primarySuffix: ".dmg",
    provenanceFile: "artifact-mac-arm64.provenance.json",
  },
  "macos-x64": {
    platform: "mac",
    arch: "x64",
    target: "dmg",
    primarySuffix: ".dmg",
    provenanceFile: "artifact-mac-x64.provenance.json",
  },
  "windows-x64": {
    platform: "win",
    arch: "x64",
    target: "nsis",
    primarySuffix: ".exe",
    provenanceFile: "artifact-win-x64.provenance.json",
  },
} as const;

type Stage6DesktopTargetId = keyof typeof TARGETS;

const VERIFIED_STAGE6_DESKTOP_ARTIFACT_SET: unique symbol = Symbol(
  "VerifiedStage6DesktopArtifactSet",
);
const VERIFIED_STAGE6_DESKTOP_ARTIFACT_SETS = new WeakSet<object>();

export interface Stage6DesktopArtifactSetInput {
  readonly assetsDirectory: string;
  readonly candidateTag: string;
  readonly sourceCommit: string;
  readonly lockfileSha256: string;
  readonly expectedArtifactSetSha256: string;
}

export interface Stage6DesktopArtifactSetTarget {
  readonly id: Stage6DesktopTargetId;
  readonly provenanceFile: string;
  readonly provenanceSha256: string;
  readonly attestationBundleFile: string;
  readonly attestationBundleSha256: string;
  readonly artifactAttestationFile: string;
  readonly artifactAttestationSha256: string;
  readonly primaryInstaller: string;
  readonly primaryInstallerSha256: string;
  readonly artifacts: ReadonlyArray<{
    readonly fileName: string;
    readonly sizeBytes: number;
    readonly sha256: string;
  }>;
}

export interface Stage6DesktopArtifactSetResult {
  readonly artifactSetSha256: string;
  readonly candidateTag: string;
  readonly sourceCommit: string;
  readonly lockfileSha256: string;
  readonly targets: ReadonlyArray<Stage6DesktopArtifactSetTarget>;
}

export interface VerifiedStage6DesktopArtifactSet extends Stage6DesktopArtifactSetResult {
  readonly [VERIFIED_STAGE6_DESKTOP_ARTIFACT_SET]: true;
}

export function assertVerifiedStage6DesktopArtifactSet(
  value: unknown,
): asserts value is VerifiedStage6DesktopArtifactSet {
  if (
    typeof value !== "object" ||
    value === null ||
    !VERIFIED_STAGE6_DESKTOP_ARTIFACT_SETS.has(value)
  ) {
    throw new Error("Stage 6 Desktop artifact set was not produced by the local byte verifier.");
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
  const actual = Object.keys(object).toSorted();
  const expected = [...fields].toSorted();
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new Error(`${label} fields do not match the release provenance schema.`);
  }
  return object;
}

function sha256(buffer: Buffer | string): string {
  return createHash("sha256").update(buffer).digest("hex");
}

function readRegularFile(path: string, label: string, maximumBytes?: number): Buffer {
  const entry = lstatSync(path);
  if (!entry.isFile() || entry.isSymbolicLink()) {
    throw new Error(`${label} must be a regular non-symlink file.`);
  }
  if (maximumBytes !== undefined && entry.size > maximumBytes) {
    throw new Error(`${label} exceeds its bounded size.`);
  }
  return readFileSync(path);
}

function hashRegularFile(
  path: string,
  label: string,
): { readonly size: number; readonly sha256: string } {
  const entry = lstatSync(path);
  if (!entry.isFile() || entry.isSymbolicLink()) {
    throw new Error(`${label} must be a regular non-symlink file.`);
  }
  const hash = createHash("sha256");
  const buffer = Buffer.allocUnsafe(1024 * 1024);
  const descriptor = openSync(path, "r");
  try {
    for (;;) {
      const bytesRead = readSync(descriptor, buffer, 0, buffer.byteLength, null);
      if (bytesRead === 0) break;
      hash.update(buffer.subarray(0, bytesRead));
    }
  } finally {
    closeSync(descriptor);
  }
  return { size: entry.size, sha256: hash.digest("hex") };
}

function requireString(value: unknown, label: string): string {
  if (typeof value !== "string" || value.length === 0) {
    throw new Error(`${label} must be a non-empty string.`);
  }
  return value;
}

export function computeStage6DesktopArtifactSetSha256(
  evidenceDigests: Readonly<
    Record<Stage6DesktopTargetId, Record<Stage6DesktopArtifactSetEvidenceField, string>>
  >,
): string {
  const lines = (Object.keys(TARGETS) as Stage6DesktopTargetId[])
    .toSorted()
    .flatMap((targetId) =>
      ARTIFACT_SET_EVIDENCE_FIELDS.map((field) => {
        const digest = evidenceDigests[targetId][field];
        if (!SHA256_PATTERN.test(digest)) {
          throw new Error(`${targetId} ${field} digest must be sha256:<64 lowercase hex>.`);
        }
        return `${targetId}/${field}=${digest}\n`;
      }),
    )
    .join("");
  return `sha256:${sha256(lines)}`;
}

export function verifyStage6DesktopArtifactSet(
  input: Stage6DesktopArtifactSetInput,
): VerifiedStage6DesktopArtifactSet {
  if (!/^v[A-Za-z0-9][A-Za-z0-9._+-]{0,198}$/.test(input.candidateTag)) {
    throw new Error("Stage 6 candidate tag is invalid.");
  }
  if (!COMMIT_PATTERN.test(input.sourceCommit)) {
    throw new Error("Stage 6 source commit must be 40 lowercase hex characters.");
  }
  if (!HEX_SHA256_PATTERN.test(input.lockfileSha256)) {
    throw new Error("Stage 6 lockfile digest must be 64 lowercase hex characters.");
  }
  if (!SHA256_PATTERN.test(input.expectedArtifactSetSha256)) {
    throw new Error("Expected Stage 6 Desktop artifact-set digest is invalid.");
  }
  const root = resolve(input.assetsDirectory);
  const rootEntry = lstatSync(root);
  if (!rootEntry.isDirectory() || rootEntry.isSymbolicLink()) {
    throw new Error("Stage 6 Desktop assets directory must be a regular non-symlink directory.");
  }

  const version = input.candidateTag.slice(1);
  const claimedArtifactFiles = new Set<string>();
  const evidenceDigests = {} as Record<
    Stage6DesktopTargetId,
    Record<Stage6DesktopArtifactSetEvidenceField, string>
  >;
  const expectedSupplementalFiles = new Set<string>();
  const targets: Stage6DesktopArtifactSetTarget[] = [];
  for (const targetId of (Object.keys(TARGETS) as Stage6DesktopTargetId[]).toSorted()) {
    const expected = TARGETS[targetId];
    const provenancePath = join(root, expected.provenanceFile);
    const provenanceBytes = readRegularFile(
      provenancePath,
      `${targetId} provenance`,
      MAX_PROVENANCE_BYTES,
    );
    const provenanceSha256 = `sha256:${sha256(provenanceBytes)}`;
    const attestationBundleFile = `artifact-${expected.platform}-${expected.arch}.attestation.json`;
    const artifactAttestationFile = `artifact-${expected.platform}-${expected.arch}.attestation-verification.json`;
    const attestationBundle = hashRegularFile(
      join(root, attestationBundleFile),
      `${targetId} attestation bundle`,
    );
    const artifactAttestation = hashRegularFile(
      join(root, artifactAttestationFile),
      `${targetId} artifact attestation verification`,
    );
    if (
      attestationBundle.size <= 0 ||
      attestationBundle.size > MAX_ATTESTATION_BYTES ||
      artifactAttestation.size <= 0 ||
      artifactAttestation.size > MAX_ATTESTATION_BYTES
    ) {
      throw new Error(`${targetId} attestation evidence exceeds its bounded size.`);
    }
    const attestationBundleSha256 = `sha256:${attestationBundle.sha256}`;
    const artifactAttestationSha256 = `sha256:${artifactAttestation.sha256}`;
    evidenceDigests[targetId] = {
      provenance: provenanceSha256,
      attestationBundle: attestationBundleSha256,
      artifactAttestation: artifactAttestationSha256,
    };
    expectedSupplementalFiles.add(expected.provenanceFile);
    expectedSupplementalFiles.add(attestationBundleFile);
    expectedSupplementalFiles.add(artifactAttestationFile);
    let decoded: unknown;
    try {
      decoded = JSON.parse(provenanceBytes.toString("utf8"));
    } catch (error) {
      throw new Error(`${targetId} provenance must be valid UTF-8 JSON.`, { cause: error });
    }
    const provenance = requireExactFields(
      decoded,
      [
        "schemaVersion",
        "publication",
        "platform",
        "arch",
        "target",
        "version",
        "source",
        "signing",
        "artifacts",
      ],
      `${targetId} provenance`,
    );
    if (
      provenance.schemaVersion !== 1 ||
      provenance.publication !== true ||
      provenance.platform !== expected.platform ||
      provenance.arch !== expected.arch ||
      provenance.target !== expected.target ||
      provenance.version !== version
    ) {
      throw new Error(`${targetId} provenance does not identify the protected publication target.`);
    }
    const source = requireExactFields(
      provenance.source,
      ["commit", "tag", "lockfileSha256"],
      `${targetId} provenance source`,
    );
    if (
      source.commit !== input.sourceCommit ||
      source.tag !== input.candidateTag ||
      source.lockfileSha256 !== input.lockfileSha256
    ) {
      throw new Error(`${targetId} provenance source does not match the protected candidate.`);
    }
    if (!Array.isArray(provenance.artifacts) || provenance.artifacts.length === 0) {
      throw new Error(`${targetId} provenance artifacts must be non-empty.`);
    }
    const primary: Array<{ fileName: string; sha256: string }> = [];
    const artifacts: Array<{ fileName: string; sizeBytes: number; sha256: string }> = [];
    for (const [index, rawArtifact] of provenance.artifacts.entries()) {
      const artifact = requireExactFields(
        rawArtifact,
        ["fileName", "size", "sha256"],
        `${targetId} provenance artifact ${index}`,
      );
      const fileName = requireString(artifact.fileName, `${targetId} artifact file name`);
      if (
        basename(fileName) !== fileName ||
        !FILE_NAME_PATTERN.test(fileName) ||
        claimedArtifactFiles.has(fileName)
      ) {
        throw new Error(
          "Stage 6 provenance artifact file names must be unique bounded base names.",
        );
      }
      const size = artifact.size;
      const digest = artifact.sha256;
      if (
        !Number.isSafeInteger(size) ||
        (size as number) <= 0 ||
        typeof digest !== "string" ||
        !HEX_SHA256_PATTERN.test(digest)
      ) {
        throw new Error(`${targetId} provenance artifact metadata is invalid.`);
      }
      const artifactFile = hashRegularFile(
        join(root, fileName),
        `${targetId} artifact ${fileName}`,
      );
      if (artifactFile.size !== size || artifactFile.sha256 !== digest) {
        throw new Error(`${targetId} artifact ${fileName} does not match its provenance.`);
      }
      claimedArtifactFiles.add(fileName);
      artifacts.push(
        Object.freeze({
          fileName,
          sizeBytes: size as number,
          sha256: `sha256:${digest}`,
        }),
      );
      if (fileName.endsWith(expected.primarySuffix)) primary.push({ fileName, sha256: digest });
    }
    if (primary.length !== 1) {
      throw new Error(`${targetId} provenance must identify exactly one primary installer.`);
    }
    targets.push(
      Object.freeze({
        id: targetId,
        provenanceFile: expected.provenanceFile,
        provenanceSha256,
        attestationBundleFile,
        attestationBundleSha256,
        artifactAttestationFile,
        artifactAttestationSha256,
        primaryInstaller: primary[0]!.fileName,
        primaryInstallerSha256: `sha256:${primary[0]!.sha256}`,
        artifacts: Object.freeze(
          artifacts.toSorted((left, right) => left.fileName.localeCompare(right.fileName)),
        ),
      }),
    );
  }

  const unexpectedPayload = readdirSync(root).find(
    (fileName) =>
      /\.(?:AppImage|blockmap|dmg|exe|zip)$/.test(fileName) && !claimedArtifactFiles.has(fileName),
  );
  if (unexpectedPayload) {
    throw new Error(`Unprovenanced Stage 6 Desktop payload found: ${unexpectedPayload}.`);
  }
  const unexpectedSupplemental = readdirSync(root).find(
    (fileName) =>
      (fileName.endsWith(".provenance.json") || /\.attestation.*\.json$/.test(fileName)) &&
      !expectedSupplementalFiles.has(fileName),
  );
  if (unexpectedSupplemental) {
    throw new Error(`Unexpected Stage 6 Desktop trust evidence found: ${unexpectedSupplemental}.`);
  }
  const artifactSetSha256 = computeStage6DesktopArtifactSetSha256(evidenceDigests);
  if (artifactSetSha256 !== input.expectedArtifactSetSha256) {
    throw new Error(
      "Downloaded Desktop artifact set does not match the approved candidate evidence.",
    );
  }
  const verified: VerifiedStage6DesktopArtifactSet = Object.freeze({
    artifactSetSha256,
    candidateTag: input.candidateTag,
    sourceCommit: input.sourceCommit,
    lockfileSha256: input.lockfileSha256,
    targets: Object.freeze(targets),
    [VERIFIED_STAGE6_DESKTOP_ARTIFACT_SET]: true as const,
  });
  VERIFIED_STAGE6_DESKTOP_ARTIFACT_SETS.add(verified);
  return verified;
}
