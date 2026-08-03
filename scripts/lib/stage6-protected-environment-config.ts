import { createHash, randomUUID } from "node:crypto";
import {
  closeSync,
  constants,
  fstatSync,
  fsyncSync,
  linkSync,
  lstatSync,
  openSync,
  readFileSync,
  realpathSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { basename, dirname, join, resolve } from "node:path";

import {
  STAGE6_CANDIDATE_RECEIPT_MAX_BYTES,
  STAGE6_CANDIDATE_RELEASE_BINDING_SCHEMA,
  verifyStage6CandidateReleaseBinding,
} from "./stage6-candidate-release-binding.ts";

const COMMIT_PATTERN = /^[0-9a-f]{40}$/;
const CANDIDATE_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._:/@+-]{1,199}$/;
const RUN_ID_PATTERN = /^[1-9][0-9]{0,19}$/;

export const STAGE6_PROTECTED_ENVIRONMENT_CONFIG_SCHEMA =
  "synara.stage6-protected-environment-configuration.v1";
export const STAGE6_PROTECTED_ENVIRONMENT_CONFIG_ASSESSMENT =
  "candidate-environment-configuration-prepared-not-approved";

export interface Stage6ProtectedEnvironmentConfigInput {
  readonly candidateReceiptPath: string;
  readonly expectedCandidateId: string;
  readonly expectedSourceCommit: string;
  readonly buildRunId: string;
}

export interface Stage6ProtectedEnvironmentConfig {
  readonly schemaVersion: typeof STAGE6_PROTECTED_ENVIRONMENT_CONFIG_SCHEMA;
  readonly environment: "stage6-enterprise-ga";
  readonly variables: {
    readonly SYNARA_STAGE6_CANDIDATE_ID: string;
    readonly SYNARA_STAGE6_CANDIDATE_SOURCE_COMMIT: string;
    readonly SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID: string;
    readonly SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_SHA256: string;
    readonly SYNARA_STAGE6_DESKTOP_ARTIFACT_SET_SHA256: string;
  };
  readonly secrets: {
    readonly SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64: string;
  };
  readonly requiredReviewComment: string;
  readonly receipt: {
    readonly fileName: string;
    readonly sizeBytes: number;
    readonly sha256: string;
  };
  readonly verification: {
    readonly candidateReleaseBindingSchema: typeof STAGE6_CANDIDATE_RELEASE_BINDING_SCHEMA;
    readonly exactReceiptBytesValidated: true;
    readonly allReceiptProjectionsValidated: true;
    readonly base64FitsGithubSecretLimit: true;
  };
  readonly assessment: typeof STAGE6_PROTECTED_ENVIRONMENT_CONFIG_ASSESSMENT;
}

function requirePattern(value: string, pattern: RegExp, label: string): string {
  if (value !== value.trim() || !pattern.test(value)) {
    throw new Error(`${label} is invalid.`);
  }
  return value;
}

function readCandidateReceipt(pathInput: string): {
  readonly path: string;
  readonly bytes: Buffer;
} {
  const path = resolve(pathInput);
  const entry = lstatSync(path);
  if (!entry.isFile() || entry.isSymbolicLink()) {
    throw new Error("Stage 6 candidate receipt must be a regular non-symlink file.");
  }
  if (entry.size > STAGE6_CANDIDATE_RECEIPT_MAX_BYTES) {
    throw new Error("Stage 6 candidate receipt exceeds its bounded size.");
  }
  const descriptor = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const opened = fstatSync(descriptor);
    if (
      !opened.isFile() ||
      opened.size !== entry.size ||
      opened.dev !== entry.dev ||
      opened.ino !== entry.ino
    ) {
      throw new Error("Stage 6 candidate receipt changed while it was opened.");
    }
    const bytes = readFileSync(descriptor);
    const finalEntry = fstatSync(descriptor);
    if (
      bytes.byteLength !== opened.size ||
      finalEntry.size !== opened.size ||
      finalEntry.mtimeMs !== opened.mtimeMs ||
      finalEntry.ctimeMs !== opened.ctimeMs
    ) {
      throw new Error("Stage 6 candidate receipt changed while it was read.");
    }
    return { path, bytes };
  } finally {
    closeSync(descriptor);
  }
}

function requireObject(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object.`);
  }
  return value as Record<string, unknown>;
}

function requireString(value: unknown, label: string): string {
  if (typeof value !== "string" || value.length === 0) {
    throw new Error(`${label} must be a non-empty string.`);
  }
  return value;
}

export function createStage6ProtectedEnvironmentConfig(
  input: Stage6ProtectedEnvironmentConfigInput,
): Stage6ProtectedEnvironmentConfig {
  const expectedCandidateId = requirePattern(
    input.expectedCandidateId,
    CANDIDATE_ID_PATTERN,
    "Expected candidate ID",
  );
  const expectedSourceCommit = requirePattern(
    input.expectedSourceCommit,
    COMMIT_PATTERN,
    "Expected source commit",
  );
  const buildRunId = requirePattern(input.buildRunId, RUN_ID_PATTERN, "Build run ID");
  const { path, bytes } = readCandidateReceipt(input.candidateReceiptPath);
  const receiptSha256 = `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
  let parsed: unknown;
  try {
    parsed = JSON.parse(bytes.toString("utf8"));
  } catch (error) {
    throw new Error("Stage 6 candidate receipt must be valid JSON.", {
      cause: error,
    });
  }
  const candidate = requireObject(
    requireObject(parsed, "Stage 6 candidate receipt").candidate,
    "Stage 6 candidate receipt candidate",
  );
  const lockfileSha256 = requireString(candidate.lockfileSha256, "candidate.lockfileSha256");
  const desktopArtifactSetSha256 = requireString(
    candidate.desktopArtifactSetSha256,
    "candidate.desktopArtifactSetSha256",
  );
  const binding = verifyStage6CandidateReleaseBinding({
    candidateReceiptPath: path,
    expectedCandidateReceiptSha256: receiptSha256,
    expectedCandidateId,
    expectedSourceCommit,
    expectedLockfileSha256: lockfileSha256,
    expectedDesktopArtifactSetSha256: desktopArtifactSetSha256,
  });
  const receiptBase64 = bytes.toString("base64");
  if (Buffer.byteLength(receiptBase64, "utf8") > 48 * 1024) {
    throw new Error("Stage 6 candidate receipt base64 exceeds the GitHub secret size limit.");
  }
  return {
    schemaVersion: STAGE6_PROTECTED_ENVIRONMENT_CONFIG_SCHEMA,
    environment: "stage6-enterprise-ga",
    variables: {
      SYNARA_STAGE6_CANDIDATE_ID: binding.candidateId,
      SYNARA_STAGE6_CANDIDATE_SOURCE_COMMIT: binding.sourceCommit,
      SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID: buildRunId,
      SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_SHA256: binding.candidateBundleReceiptSha256,
      SYNARA_STAGE6_DESKTOP_ARTIFACT_SET_SHA256: binding.desktopArtifactSetSha256,
    },
    secrets: {
      SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64: receiptBase64,
    },
    requiredReviewComment: `SYNARA_STAGE6_APPROVE ${binding.candidateId} ${buildRunId}`,
    receipt: {
      fileName: basename(path),
      sizeBytes: bytes.byteLength,
      sha256: binding.candidateBundleReceiptSha256,
    },
    verification: {
      candidateReleaseBindingSchema: STAGE6_CANDIDATE_RELEASE_BINDING_SCHEMA,
      exactReceiptBytesValidated: true,
      allReceiptProjectionsValidated: true,
      base64FitsGithubSecretLimit: true,
    },
    assessment: STAGE6_PROTECTED_ENVIRONMENT_CONFIG_ASSESSMENT,
  };
}

function encode(value: Stage6ProtectedEnvironmentConfig): Buffer {
  return Buffer.from(`${JSON.stringify(value, null, 2)}\n`, "utf8");
}

function unlinkIfPresent(path: string): unknown | undefined {
  try {
    unlinkSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") return error;
  }
  return undefined;
}

function throwPublicationFailure(
  primaryError: unknown | undefined,
  cleanupErrors: ReadonlyArray<unknown>,
  message: string,
): never {
  const errors = primaryError === undefined ? [...cleanupErrors] : [primaryError, ...cleanupErrors];
  if (errors.length === 1) throw errors[0];
  throw new AggregateError(
    errors,
    message,
    primaryError === undefined ? undefined : { cause: primaryError },
  );
}

function publishExclusive(path: string, bytes: Buffer): void {
  const parent = dirname(path);
  const parentEntry = lstatSync(parent);
  if (!parentEntry.isDirectory() || parentEntry.isSymbolicLink()) {
    throw new Error("Protected environment configuration parent must be a non-symlink directory.");
  }
  const temporary = resolve(parent, `.${basename(path)}.${process.pid}.${randomUUID()}.tmp`);
  let descriptor = -1;
  let published = false;
  let primaryError: unknown | undefined;
  const cleanupErrors: unknown[] = [];
  try {
    descriptor = openSync(temporary, "wx", 0o600);
    writeFileSync(descriptor, bytes);
    fsyncSync(descriptor);
    closeSync(descriptor);
    descriptor = -1;
    linkSync(temporary, path);
    published = true;
  } catch (error) {
    primaryError = error;
  }
  if (descriptor >= 0) {
    try {
      closeSync(descriptor);
    } catch (error) {
      cleanupErrors.push(error);
    }
  }
  const temporaryCleanupError = unlinkIfPresent(temporary);
  if (temporaryCleanupError !== undefined) cleanupErrors.push(temporaryCleanupError);
  if (primaryError === undefined && cleanupErrors.length === 0) return;
  if (published) {
    const publishedCleanupError = unlinkIfPresent(path);
    if (publishedCleanupError !== undefined) cleanupErrors.push(publishedCleanupError);
  }
  throwPublicationFailure(
    primaryError,
    cleanupErrors,
    "Protected environment configuration publication and cleanup failed.",
  );
}

export function writeStage6ProtectedEnvironmentConfig(
  outputPath: string,
  input: Stage6ProtectedEnvironmentConfigInput,
): {
  readonly path: string;
  readonly sha256Path: string;
  readonly sha256: string;
} {
  const requestedPath = resolve(outputPath);
  const path = join(realpathSync(dirname(requestedPath)), basename(requestedPath));
  const sha256Path = `${path}.sha256`;
  const configBytes = encode(createStage6ProtectedEnvironmentConfig(input));
  const sha256 = createHash("sha256").update(configBytes).digest("hex");
  const sidecarBytes = Buffer.from(`${sha256}  ${basename(path)}\n`, "utf8");
  const created: string[] = [];
  try {
    publishExclusive(path, configBytes);
    created.push(path);
    publishExclusive(sha256Path, sidecarBytes);
    created.push(sha256Path);
  } catch (error) {
    const cleanupErrors = created
      .toReversed()
      .map(unlinkIfPresent)
      .filter((cleanupError): cleanupError is unknown => cleanupError !== undefined);
    if (cleanupErrors.length > 0) {
      throwPublicationFailure(
        error,
        cleanupErrors,
        "Protected environment configuration rollback failed.",
      );
    }
    throw error;
  }
  return { path, sha256Path, sha256 };
}
