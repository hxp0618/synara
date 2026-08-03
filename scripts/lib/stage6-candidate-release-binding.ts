import { createHash } from "node:crypto";
import { closeSync, constants, fstatSync, lstatSync, openSync, readSync } from "node:fs";
import { resolve } from "node:path";
import { TextDecoder } from "node:util";

const COMMIT_PATTERN = /^[0-9a-f]{40}$/;
const HEX_SHA256_PATTERN = /^[0-9a-f]{64}$/;
const SHA256_PATTERN = /^sha256:[0-9a-f]{64}$/;
const CANDIDATE_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._:/@+-]{1,199}$/;
const MIGRATION_NAME_PATTERN = /^\d{6}_[a-z0-9_]+\.sql$/;
const UTC_TIMESTAMP_PATTERN = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$/;
const ZERO_HEX_SHA256 = "0".repeat(64);
const REQUIRED_RECEIPTS = [
  "internalCost",
  "capacity",
  "desktop",
  "incident",
  "operations",
  "penetration",
  "recovery",
  "residency",
  "slo",
  "workerSupplyChain",
] as const;
const RECEIPT_PROJECTION_BASE_FIELDS = [
  "assessment",
  "path",
  "readyForCandidateReview",
  "schemaVersion",
  "sha256",
  "validatedAt",
] as const;
const RECEIPT_PROJECTION_SPECS = {
  internalCost: {
    schemaVersion: "synara.stage6-internal-cost-evidence-validation.v1",
    assessment: "evidence-validated-not-internal-cost-approved",
    extraFields: [],
  },
  capacity: {
    schemaVersion: "synara.capacity-soak-evidence-receipt.v1",
    assessment: "evidence-validated-not-capacity-passed",
    extraFields: [],
  },
  desktop: {
    schemaVersion: "synara.stage6-desktop-native-acceptance-validation.v1",
    assessment: "evidence-validated-not-desktop-ga-passed",
    extraFields: ["desktopArtifactSetSha256", "desktopEnrollmentEvidenceSetSha256"],
  },
  incident: {
    schemaVersion: "synara.incident-communication-exercise-evidence-receipt.v2",
    assessment: "evidence-validated-not-operations-ready",
    extraFields: [],
  },
  operations: {
    schemaVersion: "synara.stage6-operations-browser-exercise-validation.v2",
    assessment: "evidence-validated-not-operations-passed",
    extraFields: [],
  },
  penetration: {
    schemaVersion: "synara.third-party-penetration-evidence-receipt.v1",
    assessment: "evidence-validated-not-penetration-passed",
    extraFields: [],
  },
  recovery: {
    schemaVersion: "synara.recovery-drill-evidence-receipt.v2",
    assessment: "evidence-validated-not-control-passed",
    extraFields: [
      "candidateBindingSha256",
      "cryptographicSignaturesVerified",
      "realBackupRestoreAndApproverAuthorityVerificationRequired",
      "recoverySubjectSha256",
    ],
  },
  residency: {
    schemaVersion: "synara.data-residency-deployment-evidence-receipt.v1",
    assessment: "evidence-validated-not-residency-approved",
    extraFields: [
      "allowedRegions",
      "candidateBindingSha256",
      "cryptographicSignaturesVerified",
      "evidenceSetSha256",
      "externalSignatureIdentityAndAuthorityVerificationRequired",
      "homeRegion",
      "policyDigest",
      "policyVersion",
    ],
  },
  slo: {
    schemaVersion: "synara.slo-window-evidence-receipt.v1",
    assessment: "evidence-validated-not-slo-passed",
    extraFields: [],
  },
  workerSupplyChain: {
    schemaVersion: "synara.stage6-worker-supply-chain-evidence.v1",
    assessment: "evidence-validated-not-worker-supply-chain-approved",
    extraFields: ["admissionReportSha256", "registryReportSha256"],
  },
} as const satisfies Record<
  (typeof REQUIRED_RECEIPTS)[number],
  {
    readonly schemaVersion: string;
    readonly assessment: string;
    readonly extraFields: ReadonlyArray<string>;
  }
>;
const DESKTOP_TARGETS = ["linux-x64", "macos-arm64", "macos-x64", "windows-x64"] as const;

export const STAGE6_CANDIDATE_RECEIPT_SCHEMA =
  "synara.stage6-candidate-evidence-bundle-validation.v5";
export const STAGE6_CANDIDATE_RECEIPT_ASSESSMENT = "evidence-consistent-not-ga-approved";
export const STAGE6_CANDIDATE_RELEASE_BINDING_SCHEMA = "synara.stage6-candidate-release-binding.v2";
export const STAGE6_CANDIDATE_RELEASE_BINDING_ASSESSMENT =
  "candidate-receipt-verified-not-ga-approved";
export const STAGE6_CANDIDATE_RECEIPT_MAX_BYTES = 32 * 1024;

const VERIFIED_STAGE6_CANDIDATE_RELEASE_BINDING: unique symbol = Symbol(
  "VerifiedStage6CandidateReleaseBinding",
);
const VERIFIED_STAGE6_CANDIDATE_RELEASE_BINDINGS = new WeakSet<object>();

export interface Stage6CandidateReleaseBindingInput {
  readonly candidateReceiptPath: string;
  readonly expectedCandidateReceiptSha256: string;
  readonly expectedCandidateId: string;
  readonly expectedSourceCommit: string;
  readonly expectedLockfileSha256: string;
  readonly expectedDesktopArtifactSetSha256: string;
}

export interface Stage6CandidateReleaseBindingValue {
  readonly schemaVersion: typeof STAGE6_CANDIDATE_RELEASE_BINDING_SCHEMA;
  readonly candidateId: string;
  readonly sourceCommit: string;
  readonly lockfileSha256: string;
  readonly desktopArtifactSetSha256: string;
  readonly candidateBundleReceiptSha256: string;
  readonly receipt: {
    readonly path: string;
    readonly sizeBytes: number;
    readonly schemaVersion: typeof STAGE6_CANDIDATE_RECEIPT_SCHEMA;
  };
  readonly verification: {
    readonly receiptDigestMatched: true;
    readonly receiptSchemaValidated: true;
    readonly allReceiptProjectionsValidated: true;
    readonly workerSupplyChainReceiptValidated: true;
    readonly candidateIdentityMatched: true;
    readonly candidateEligibilityValidated: true;
  };
  readonly assessment: typeof STAGE6_CANDIDATE_RELEASE_BINDING_ASSESSMENT;
}

export interface VerifiedStage6CandidateReleaseBinding extends Stage6CandidateReleaseBindingValue {
  readonly [VERIFIED_STAGE6_CANDIDATE_RELEASE_BINDING]: true;
}

export function assertVerifiedStage6CandidateReleaseBinding(
  value: unknown,
): asserts value is VerifiedStage6CandidateReleaseBinding {
  if (
    typeof value !== "object" ||
    value === null ||
    !VERIFIED_STAGE6_CANDIDATE_RELEASE_BINDINGS.has(value)
  ) {
    throw new Error(
      "Stage 6 candidate release binding was not produced by the local receipt verifier.",
    );
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
    throw new Error(`${label} fields do not match the Stage 6 candidate receipt schema.`);
  }
  return object;
}

function requireString(value: unknown, label: string): string {
  if (typeof value !== "string" || value.length === 0 || value !== value.trim()) {
    throw new Error(`${label} must be a non-empty string without surrounding whitespace.`);
  }
  return value;
}

function requirePattern(value: string, pattern: RegExp, label: string): string {
  const normalized = requireString(value, label);
  if (!pattern.test(normalized)) throw new Error(`${label} is invalid.`);
  return normalized;
}

function requireNonZeroHexSha256(value: string, label: string): string {
  const normalized = requirePattern(value, HEX_SHA256_PATTERN, label);
  if (normalized === ZERO_HEX_SHA256) throw new Error(`${label} must not be an all-zero digest.`);
  return normalized;
}

function requireNonZeroSha256(value: string, label: string): string {
  const normalized = requirePattern(value, SHA256_PATTERN, label);
  if (normalized === `sha256:${ZERO_HEX_SHA256}`) {
    throw new Error(`${label} must not be an all-zero digest.`);
  }
  return normalized;
}

function requireTrue(value: unknown, label: string): true {
  if (value !== true) throw new Error(`${label} must be true before protected release approval.`);
  return true;
}

function requireFalse(value: unknown, label: string): false {
  if (value !== false) throw new Error(`${label} must be false.`);
  return false;
}

function requireUtcTimestamp(value: unknown, label: string): string {
  const timestamp = requireString(value, label);
  if (!UTC_TIMESTAMP_PATTERN.test(timestamp) || Number.isNaN(Date.parse(timestamp))) {
    throw new Error(`${label} must be a valid RFC3339 UTC timestamp.`);
  }
  return timestamp;
}

function requireRelativePath(value: unknown, label: string): string {
  const path = requireString(value, label);
  const parts = path.split("/");
  if (
    path.startsWith("/") ||
    path.includes("\\") ||
    /[\u0000-\u001f\u007f]/.test(path) ||
    parts.some((part) => part.length === 0 || part === "." || part === "..")
  ) {
    throw new Error(`${label} must be a traversal-free relative POSIX path.`);
  }
  return path;
}

function requireIdentifierArray(value: unknown, label: string): ReadonlyArray<string> {
  if (!Array.isArray(value) || value.length === 0) {
    throw new Error(`${label} must be a non-empty array.`);
  }
  const identifiers = value.map((item, index) =>
    requirePattern(
      requireString(item, `${label}[${index}]`),
      CANDIDATE_ID_PATTERN,
      `${label}[${index}]`,
    ),
  );
  if (new Set(identifiers).size !== identifiers.length) {
    throw new Error(`${label} must not contain duplicates.`);
  }
  if (JSON.stringify(identifiers) !== JSON.stringify([...identifiers].toSorted())) {
    throw new Error(`${label} must use canonical sorted order.`);
  }
  return Object.freeze(identifiers);
}

function requireHttpsUrl(value: unknown, label: string, originOnly = false): string {
  const raw = requireString(value, label);
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch (error) {
    throw new Error(`${label} must be a valid HTTPS URL.`, { cause: error });
  }
  if (
    parsed.protocol !== "https:" ||
    parsed.username.length > 0 ||
    parsed.password.length > 0 ||
    parsed.search.length > 0 ||
    parsed.hash.length > 0 ||
    (originOnly && parsed.pathname !== "/")
  ) {
    throw new Error(`${label} must be a credential-free HTTPS${originOnly ? " origin" : " URL"}.`);
  }
  return raw;
}

function readBoundedRegularFile(inputPath: string): {
  readonly path: string;
  readonly bytes: Buffer;
} {
  const path = resolve(inputPath);
  let entry;
  try {
    entry = lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new Error(`Stage 6 candidate receipt does not exist: ${path}`, { cause: error });
    }
    throw error;
  }
  if (!entry.isFile() || entry.isSymbolicLink()) {
    throw new Error("Stage 6 candidate receipt must be a regular non-symlink file.");
  }
  if (entry.size > STAGE6_CANDIDATE_RECEIPT_MAX_BYTES) {
    throw new Error("Stage 6 candidate receipt exceeds its bounded size.");
  }

  const descriptor = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const openedEntry = fstatSync(descriptor);
    if (!openedEntry.isFile()) {
      throw new Error("Stage 6 candidate receipt must remain a regular file while verified.");
    }
    if (openedEntry.size > STAGE6_CANDIDATE_RECEIPT_MAX_BYTES) {
      throw new Error("Stage 6 candidate receipt exceeds its bounded size.");
    }
    if (openedEntry.dev !== entry.dev || openedEntry.ino !== entry.ino) {
      throw new Error("Stage 6 candidate receipt changed while it was opened.");
    }

    const bytes = Buffer.allocUnsafe(openedEntry.size);
    let offset = 0;
    while (offset < bytes.byteLength) {
      const bytesRead = readSync(descriptor, bytes, offset, bytes.byteLength - offset, offset);
      if (bytesRead === 0) {
        throw new Error("Stage 6 candidate receipt changed while it was read.");
      }
      offset += bytesRead;
    }
    const trailingByte = Buffer.allocUnsafe(1);
    if (readSync(descriptor, trailingByte, 0, 1, offset) !== 0) {
      throw new Error("Stage 6 candidate receipt changed while it was read.");
    }
    const finalEntry = fstatSync(descriptor);
    if (
      finalEntry.size !== openedEntry.size ||
      finalEntry.mtimeMs !== openedEntry.mtimeMs ||
      finalEntry.ctimeMs !== openedEntry.ctimeMs
    ) {
      throw new Error("Stage 6 candidate receipt changed while it was read.");
    }
    return { path, bytes };
  } finally {
    closeSync(descriptor);
  }
}

function parseReceipt(bytes: Buffer): Record<string, unknown> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
  } catch {
    throw new Error("Stage 6 candidate receipt must be valid UTF-8 JSON.");
  }
  return requireExactFields(
    parsed,
    [
      "allRequiredReceiptsReadyForCandidateReview",
      "assessment",
      "candidate",
      "candidateConsistencyValidated",
      "compatibilityMatrix",
      "eligibleForCandidateEvidenceReview",
      "environmentEligible",
      "manifest",
      "receipts",
      "releaseEvidence",
      "requiredReceiptCount",
      "schemaVersion",
      "validatedAt",
    ],
    "Stage 6 candidate receipt",
  );
}

interface ValidatedCandidateProjection {
  readonly candidateId: string;
  readonly sourceCommit: string;
  readonly lockfileSha256: string;
  readonly desktopArtifactSetSha256: string;
  readonly regions: ReadonlyArray<string>;
  readonly migrationTail: { readonly name: string; readonly sha256: string };
}

function validateCandidateProjection(value: unknown): ValidatedCandidateProjection {
  const candidate = requireExactFields(
    value,
    [
      "artifacts",
      "candidateId",
      "desktopArtifactSetSha256",
      "environmentClass",
      "environmentId",
      "lockfileSha256",
      "migrationTail",
      "origins",
      "regions",
      "sourceCommit",
    ],
    "Stage 6 candidate receipt candidate",
  );
  const candidateId = requirePattern(
    requireString(candidate.candidateId, "candidate.candidateId"),
    CANDIDATE_ID_PATTERN,
    "candidate.candidateId",
  );
  const sourceCommit = requirePattern(
    requireString(candidate.sourceCommit, "candidate.sourceCommit"),
    COMMIT_PATTERN,
    "candidate.sourceCommit",
  );
  const lockfileSha256 = requireNonZeroHexSha256(
    requireString(candidate.lockfileSha256, "candidate.lockfileSha256"),
    "candidate.lockfileSha256",
  );
  const desktopArtifactSetSha256 = requireNonZeroSha256(
    requireString(candidate.desktopArtifactSetSha256, "candidate.desktopArtifactSetSha256"),
    "candidate.desktopArtifactSetSha256",
  );
  if (!new Set(["production", "production-like"]).has(candidate.environmentClass as string)) {
    throw new Error("candidate.environmentClass must be release eligible.");
  }
  requirePattern(
    requireString(candidate.environmentId, "candidate.environmentId"),
    CANDIDATE_ID_PATTERN,
    "candidate.environmentId",
  );
  const regions = requireIdentifierArray(candidate.regions, "candidate.regions");

  const origins = requireExactFields(
    candidate.origins,
    ["adminBaseUrl", "controlPlaneBaseUrl", "webBaseUrl"],
    "candidate.origins",
  );
  requireHttpsUrl(origins.controlPlaneBaseUrl, "candidate.origins.controlPlaneBaseUrl");
  const webBaseUrl = requireHttpsUrl(origins.webBaseUrl, "candidate.origins.webBaseUrl", true);
  const adminBaseUrl = requireHttpsUrl(
    origins.adminBaseUrl,
    "candidate.origins.adminBaseUrl",
    true,
  );
  if (webBaseUrl === adminBaseUrl) {
    throw new Error("candidate Web and Platform Admin origins must be distinct.");
  }

  const artifacts = requireExactFields(
    candidate.artifacts,
    [
      "adminArtifact",
      "controlPlaneImage",
      "desktopArtifacts",
      "providerHostImage",
      "webArtifact",
      "workerImage",
    ],
    "candidate.artifacts",
  );
  for (const field of [
    "adminArtifact",
    "controlPlaneImage",
    "providerHostImage",
    "webArtifact",
    "workerImage",
  ]) {
    requireNonZeroSha256(
      requireString(artifacts[field], `candidate.artifacts.${field}`),
      `candidate.artifacts.${field}`,
    );
  }
  const desktopArtifacts = requireExactFields(
    artifacts.desktopArtifacts,
    DESKTOP_TARGETS,
    "candidate.artifacts.desktopArtifacts",
  );
  for (const target of DESKTOP_TARGETS) {
    requireNonZeroSha256(
      requireString(desktopArtifacts[target], `candidate.artifacts.desktopArtifacts.${target}`),
      `candidate.artifacts.desktopArtifacts.${target}`,
    );
  }

  const migrationTail = requireExactFields(
    candidate.migrationTail,
    ["name", "sha256"],
    "candidate.migrationTail",
  );
  const migrationName = requirePattern(
    requireString(migrationTail.name, "candidate.migrationTail.name"),
    MIGRATION_NAME_PATTERN,
    "candidate.migrationTail.name",
  );
  const migrationSha256 = requireNonZeroSha256(
    requireString(migrationTail.sha256, "candidate.migrationTail.sha256"),
    "candidate.migrationTail.sha256",
  );

  return {
    candidateId,
    sourceCommit,
    lockfileSha256,
    desktopArtifactSetSha256,
    regions,
    migrationTail: { name: migrationName, sha256: migrationSha256 },
  };
}

function validateCompatibilityMatrix(
  value: unknown,
  candidate: ValidatedCandidateProjection,
): string {
  const compatibility = requireExactFields(
    value,
    [
      "assessment",
      "matrixVersion",
      "migrationTail",
      "path",
      "schemaVersion",
      "sha256",
      "sourceByteCount",
      "sourceFileCount",
    ],
    "Stage 6 candidate receipt compatibilityMatrix",
  );
  const path = requireRelativePath(compatibility.path, "compatibilityMatrix.path");
  requireNonZeroSha256(
    requireString(compatibility.sha256, "compatibilityMatrix.sha256"),
    "compatibilityMatrix.sha256",
  );
  if (compatibility.schemaVersion !== "synara.release-compatibility-matrix.v1") {
    throw new Error("Stage 6 compatibility matrix schemaVersion is unsupported.");
  }
  if (compatibility.assessment !== "source-compatible-not-release-approved") {
    throw new Error("Stage 6 compatibility matrix assessment is not the reviewed non-pass value.");
  }
  if (compatibility.matrixVersion !== 1) {
    throw new Error("Stage 6 compatibility matrix version is unsupported.");
  }
  for (const field of ["sourceFileCount", "sourceByteCount"] as const) {
    if (!Number.isSafeInteger(compatibility[field]) || (compatibility[field] as number) < 1) {
      throw new Error(`compatibilityMatrix.${field} must be a positive safe integer.`);
    }
  }
  const migrationTail = requireExactFields(
    compatibility.migrationTail,
    ["name", "sha256"],
    "compatibilityMatrix.migrationTail",
  );
  if (
    migrationTail.name !== candidate.migrationTail.name ||
    migrationTail.sha256 !== candidate.migrationTail.sha256
  ) {
    throw new Error("Stage 6 compatibility matrix migration tail does not match the candidate.");
  }
  return path;
}

function validateReceiptProjections(
  value: unknown,
  candidate: ValidatedCandidateProjection,
  receiptValidatedAt: string,
  usedPaths: Set<string>,
): void {
  const projectedReceipts = requireExactFields(
    value,
    REQUIRED_RECEIPTS,
    "Stage 6 candidate receipt receipts",
  );
  for (const name of REQUIRED_RECEIPTS) {
    const spec = RECEIPT_PROJECTION_SPECS[name];
    const projection = requireExactFields(
      projectedReceipts[name],
      [...RECEIPT_PROJECTION_BASE_FIELDS, ...spec.extraFields],
      `Stage 6 candidate ${name} receipt`,
    );
    const path = requireRelativePath(projection.path, `${name}.path`);
    if (usedPaths.has(path)) {
      throw new Error("Stage 6 candidate receipt projections must use distinct evidence paths.");
    }
    usedPaths.add(path);
    requireNonZeroSha256(requireString(projection.sha256, `${name}.sha256`), `${name}.sha256`);
    if (projection.schemaVersion !== spec.schemaVersion) {
      throw new Error(`Stage 6 candidate ${name} receipt schemaVersion is unsupported.`);
    }
    if (projection.assessment !== spec.assessment) {
      throw new Error(
        `Stage 6 candidate ${name} receipt assessment is not the reviewed non-pass value.`,
      );
    }
    requireTrue(projection.readyForCandidateReview, `${name}.readyForCandidateReview`);
    const validatedAt = requireUtcTimestamp(projection.validatedAt, `${name}.validatedAt`);
    if (Date.parse(validatedAt) > Date.parse(receiptValidatedAt)) {
      throw new Error(`${name}.validatedAt must not follow the candidate receipt validation.`);
    }

    if (name === "desktop") {
      const projectedDigest = requireNonZeroSha256(
        requireString(projection.desktopArtifactSetSha256, "desktop.desktopArtifactSetSha256"),
        "desktop.desktopArtifactSetSha256",
      );
      if (projectedDigest !== candidate.desktopArtifactSetSha256) {
        throw new Error("Desktop receipt projection does not match the candidate artifact set.");
      }
      requireNonZeroSha256(
        requireString(
          projection.desktopEnrollmentEvidenceSetSha256,
          "desktop.desktopEnrollmentEvidenceSetSha256",
        ),
        "desktop.desktopEnrollmentEvidenceSetSha256",
      );
    } else if (name === "recovery") {
      requireNonZeroSha256(
        requireString(projection.candidateBindingSha256, "recovery.candidateBindingSha256"),
        "recovery.candidateBindingSha256",
      );
      requireNonZeroSha256(
        requireString(projection.recoverySubjectSha256, "recovery.recoverySubjectSha256"),
        "recovery.recoverySubjectSha256",
      );
      requireFalse(
        projection.cryptographicSignaturesVerified,
        "recovery.cryptographicSignaturesVerified",
      );
      requireTrue(
        projection.realBackupRestoreAndApproverAuthorityVerificationRequired,
        "recovery.realBackupRestoreAndApproverAuthorityVerificationRequired",
      );
    } else if (name === "residency") {
      requireNonZeroSha256(
        requireString(projection.candidateBindingSha256, "residency.candidateBindingSha256"),
        "residency.candidateBindingSha256",
      );
      requireNonZeroSha256(
        requireString(projection.evidenceSetSha256, "residency.evidenceSetSha256"),
        "residency.evidenceSetSha256",
      );
      requireNonZeroSha256(
        requireString(projection.policyDigest, "residency.policyDigest"),
        "residency.policyDigest",
      );
      if (!Number.isInteger(projection.policyVersion) || (projection.policyVersion as number) < 1) {
        throw new Error("residency.policyVersion must be an integer greater than zero.");
      }
      const homeRegion = requirePattern(
        requireString(projection.homeRegion, "residency.homeRegion"),
        CANDIDATE_ID_PATTERN,
        "residency.homeRegion",
      );
      const allowedRegions = requireIdentifierArray(
        projection.allowedRegions,
        "residency.allowedRegions",
      );
      if (JSON.stringify(allowedRegions) !== JSON.stringify(candidate.regions)) {
        throw new Error("Residency receipt projection does not match the candidate Regions.");
      }
      if (!allowedRegions.includes(homeRegion)) {
        throw new Error("Residency home Region must be included in the allowed Regions.");
      }
      requireFalse(
        projection.cryptographicSignaturesVerified,
        "residency.cryptographicSignaturesVerified",
      );
      requireTrue(
        projection.externalSignatureIdentityAndAuthorityVerificationRequired,
        "residency.externalSignatureIdentityAndAuthorityVerificationRequired",
      );
    } else if (name === "workerSupplyChain") {
      requireNonZeroSha256(
        requireString(projection.registryReportSha256, "workerSupplyChain.registryReportSha256"),
        "workerSupplyChain.registryReportSha256",
      );
      requireNonZeroSha256(
        requireString(projection.admissionReportSha256, "workerSupplyChain.admissionReportSha256"),
        "workerSupplyChain.admissionReportSha256",
      );
    }
  }
}

export function verifyStage6CandidateReleaseBinding(
  input: Stage6CandidateReleaseBindingInput,
): VerifiedStage6CandidateReleaseBinding {
  const expectedReceiptSha256 = requireNonZeroSha256(
    input.expectedCandidateReceiptSha256,
    "Expected Stage 6 candidate receipt SHA-256",
  );
  const expectedCandidateId = requirePattern(
    input.expectedCandidateId,
    CANDIDATE_ID_PATTERN,
    "Expected Stage 6 candidate ID",
  );
  const expectedSourceCommit = requirePattern(
    input.expectedSourceCommit,
    COMMIT_PATTERN,
    "Expected Stage 6 source commit",
  );
  const expectedLockfileSha256 = requireNonZeroHexSha256(
    input.expectedLockfileSha256,
    "Expected Stage 6 lockfile SHA-256",
  );
  const expectedDesktopArtifactSetSha256 = requireNonZeroSha256(
    input.expectedDesktopArtifactSetSha256,
    "Expected Stage 6 Desktop artifact-set SHA-256",
  );

  const { path, bytes } = readBoundedRegularFile(input.candidateReceiptPath);
  const actualReceiptSha256 = `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
  if (actualReceiptSha256 !== expectedReceiptSha256) {
    throw new Error("Stage 6 candidate receipt SHA-256 does not match the expected digest.");
  }

  const receipt = parseReceipt(bytes);
  if (receipt.schemaVersion !== STAGE6_CANDIDATE_RECEIPT_SCHEMA) {
    throw new Error("Stage 6 candidate receipt schemaVersion is unsupported.");
  }
  if (receipt.assessment !== STAGE6_CANDIDATE_RECEIPT_ASSESSMENT) {
    throw new Error("Stage 6 candidate receipt assessment is not the reviewed non-pass value.");
  }
  requireTrue(receipt.candidateConsistencyValidated, "candidateConsistencyValidated");
  requireTrue(receipt.environmentEligible, "environmentEligible");
  requireTrue(
    receipt.allRequiredReceiptsReadyForCandidateReview,
    "allRequiredReceiptsReadyForCandidateReview",
  );
  requireTrue(receipt.eligibleForCandidateEvidenceReview, "eligibleForCandidateEvidenceReview");
  if (receipt.requiredReceiptCount !== REQUIRED_RECEIPTS.length) {
    throw new Error(`Stage 6 candidate receipt must bind ${REQUIRED_RECEIPTS.length} receipts.`);
  }
  const receiptValidatedAt = requireUtcTimestamp(receipt.validatedAt, "validatedAt");
  const manifest = requireExactFields(
    receipt.manifest,
    ["path", "sha256"],
    "Stage 6 candidate receipt manifest",
  );
  requireRelativePath(manifest.path, "manifest.path");
  requireNonZeroSha256(requireString(manifest.sha256, "manifest.sha256"), "manifest.sha256");
  const candidate = validateCandidateProjection(receipt.candidate);
  const compatibilityPath = validateCompatibilityMatrix(receipt.compatibilityMatrix, candidate);
  const releaseEvidence = requireExactFields(
    receipt.releaseEvidence,
    ["assessment", "generatedAt", "path", "schemaVersion", "sha256"],
    "Stage 6 candidate receipt releaseEvidence",
  );
  const releaseEvidencePath = requireRelativePath(releaseEvidence.path, "releaseEvidence.path");
  requireNonZeroSha256(
    requireString(releaseEvidence.sha256, "releaseEvidence.sha256"),
    "releaseEvidence.sha256",
  );
  if (releaseEvidence.schemaVersion !== "synara-stage6-release-evidence-v2") {
    throw new Error("Stage 6 release evidence schemaVersion is unsupported.");
  }
  if (releaseEvidence.assessment !== "evidence-collected-not-control-passed") {
    throw new Error("Stage 6 release evidence assessment is not the reviewed non-pass value.");
  }
  const releaseGeneratedAt = requireUtcTimestamp(
    releaseEvidence.generatedAt,
    "releaseEvidence.generatedAt",
  );
  if (Date.parse(releaseGeneratedAt) > Date.parse(receiptValidatedAt)) {
    throw new Error("releaseEvidence.generatedAt must not follow candidate receipt validation.");
  }

  validateReceiptProjections(
    receipt.receipts,
    candidate,
    receiptValidatedAt,
    new Set([releaseEvidencePath, compatibilityPath]),
  );
  const { candidateId, sourceCommit, lockfileSha256, desktopArtifactSetSha256 } = candidate;
  if (candidateId !== expectedCandidateId) {
    throw new Error("Stage 6 candidate receipt candidateId does not match the release candidate.");
  }
  if (sourceCommit !== expectedSourceCommit) {
    throw new Error("Stage 6 candidate receipt sourceCommit does not match the release source.");
  }
  if (lockfileSha256 !== expectedLockfileSha256) {
    throw new Error("Stage 6 candidate receipt lockfileSha256 does not match the release source.");
  }
  if (desktopArtifactSetSha256 !== expectedDesktopArtifactSetSha256) {
    throw new Error(
      "Stage 6 candidate receipt desktopArtifactSetSha256 does not match the approved artifact set.",
    );
  }

  const value: Stage6CandidateReleaseBindingValue = {
    schemaVersion: STAGE6_CANDIDATE_RELEASE_BINDING_SCHEMA,
    candidateId,
    sourceCommit,
    lockfileSha256,
    desktopArtifactSetSha256,
    candidateBundleReceiptSha256: actualReceiptSha256,
    receipt: Object.freeze({
      path,
      sizeBytes: bytes.byteLength,
      schemaVersion: STAGE6_CANDIDATE_RECEIPT_SCHEMA,
    }),
    verification: Object.freeze({
      receiptDigestMatched: true,
      receiptSchemaValidated: true,
      allReceiptProjectionsValidated: true,
      workerSupplyChainReceiptValidated: true,
      candidateIdentityMatched: true,
      candidateEligibilityValidated: true,
    }),
    assessment: STAGE6_CANDIDATE_RELEASE_BINDING_ASSESSMENT,
  };
  Object.defineProperty(value, VERIFIED_STAGE6_CANDIDATE_RELEASE_BINDING, {
    value: true,
    enumerable: false,
    writable: false,
    configurable: false,
  });
  const verified = Object.freeze(value) as VerifiedStage6CandidateReleaseBinding;
  VERIFIED_STAGE6_CANDIDATE_RELEASE_BINDINGS.add(verified);
  return verified;
}
