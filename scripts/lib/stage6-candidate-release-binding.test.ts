import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import {
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  truncateSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, it } from "vitest";
import {
  STAGE6_CANDIDATE_RECEIPT_ASSESSMENT,
  STAGE6_CANDIDATE_RECEIPT_MAX_BYTES,
  STAGE6_CANDIDATE_RECEIPT_SCHEMA,
  assertVerifiedStage6CandidateReleaseBinding,
  verifyStage6CandidateReleaseBinding,
} from "./stage6-candidate-release-binding.ts";

const candidateId = "v0.6.3-stage6.1";
const sourceCommit = "a".repeat(40);
const lockfileSha256 = "b".repeat(64);
const desktopArtifactSetSha256 = `sha256:${"d".repeat(64)}`;
const desktopEnrollmentEvidenceSetSha256 = `sha256:${"e".repeat(64)}`;
const temporaryDirectories: string[] = [];
const verifierScript = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "../verify-stage6-candidate-release-binding.ts",
);
const artifactDigest = (character: string): string => `sha256:${character.repeat(64)}`;

function receiptProjection(
  name: string,
  schemaVersion: string,
  assessment: string,
  extra: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    path: `${name}-receipt.json`,
    sha256: artifactDigest("1"),
    schemaVersion,
    assessment,
    readyForCandidateReview: true,
    validatedAt: "2026-08-01T00:00:00Z",
    ...extra,
  };
}

function receiptProjections(): Record<string, unknown> {
  return {
    internalCost: receiptProjection(
      "internalCost",
      "synara.stage6-internal-cost-evidence-validation.v1",
      "evidence-validated-not-internal-cost-approved",
    ),
    capacity: receiptProjection(
      "capacity",
      "synara.capacity-soak-evidence-receipt.v1",
      "evidence-validated-not-capacity-passed",
    ),
    desktop: receiptProjection(
      "desktop",
      "synara.stage6-desktop-native-acceptance-validation.v1",
      "evidence-validated-not-desktop-ga-passed",
      { desktopArtifactSetSha256, desktopEnrollmentEvidenceSetSha256 },
    ),
    incident: receiptProjection(
      "incident",
      "synara.incident-communication-exercise-evidence-receipt.v2",
      "evidence-validated-not-operations-ready",
    ),
    operations: receiptProjection(
      "operations",
      "synara.stage6-operations-browser-exercise-validation.v2",
      "evidence-validated-not-operations-passed",
    ),
    penetration: receiptProjection(
      "penetration",
      "synara.third-party-penetration-evidence-receipt.v1",
      "evidence-validated-not-penetration-passed",
    ),
    recovery: receiptProjection(
      "recovery",
      "synara.recovery-drill-evidence-receipt.v2",
      "evidence-validated-not-control-passed",
      {
        candidateBindingSha256: artifactDigest("2"),
        recoverySubjectSha256: artifactDigest("3"),
        cryptographicSignaturesVerified: false,
        realBackupRestoreAndApproverAuthorityVerificationRequired: true,
      },
    ),
    residency: receiptProjection(
      "residency",
      "synara.data-residency-deployment-evidence-receipt.v1",
      "evidence-validated-not-residency-approved",
      {
        candidateBindingSha256: artifactDigest("4"),
        evidenceSetSha256: artifactDigest("5"),
        policyDigest: artifactDigest("6"),
        policyVersion: 1,
        homeRegion: "us-east-1",
        allowedRegions: ["us-east-1"],
        cryptographicSignaturesVerified: false,
        externalSignatureIdentityAndAuthorityVerificationRequired: true,
      },
    ),
    slo: receiptProjection(
      "slo",
      "synara.slo-window-evidence-receipt.v1",
      "evidence-validated-not-slo-passed",
    ),
    workerSupplyChain: receiptProjection(
      "workerSupplyChain",
      "synara.stage6-worker-supply-chain-evidence.v1",
      "evidence-validated-not-worker-supply-chain-approved",
      {
        registryReportSha256: artifactDigest("7"),
        admissionReportSha256: artifactDigest("8"),
      },
    ),
  };
}

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    rmSync(directory, { recursive: true, force: true });
  }
});

function createReceipt(): Record<string, unknown> {
  return {
    schemaVersion: STAGE6_CANDIDATE_RECEIPT_SCHEMA,
    manifest: { path: "bundle.json", sha256: `sha256:${"1".repeat(64)}` },
    candidate: {
      candidateId,
      sourceCommit,
      lockfileSha256,
      environmentClass: "production-like",
      environmentId: "stage6/production-like",
      regions: ["us-east-1"],
      origins: {
        controlPlaneBaseUrl: "https://control.example.test/v1",
        webBaseUrl: "https://app.example.test",
        adminBaseUrl: "https://admin.example.test",
      },
      artifacts: {
        controlPlaneImage: artifactDigest("1"),
        workerImage: artifactDigest("2"),
        providerHostImage: artifactDigest("3"),
        webArtifact: artifactDigest("4"),
        adminArtifact: artifactDigest("5"),
        desktopArtifacts: {
          "linux-x64": artifactDigest("6"),
          "macos-arm64": artifactDigest("7"),
          "macos-x64": artifactDigest("8"),
          "windows-x64": artifactDigest("9"),
        },
      },
      migrationTail: {
        name: "000134_stage6_worker_supply_chain_candidate_evidence.sql",
        sha256: artifactDigest("a"),
      },
      desktopArtifactSetSha256,
    },
    releaseEvidence: {
      path: "release-evidence.json",
      sha256: artifactDigest("b"),
      schemaVersion: "synara-stage6-release-evidence-v2",
      assessment: "evidence-collected-not-control-passed",
      generatedAt: "2026-07-31T23:00:00Z",
    },
    compatibilityMatrix: {
      path: "compatibility-matrix.json",
      sha256: artifactDigest("c"),
      schemaVersion: "synara.release-compatibility-matrix.v1",
      matrixVersion: 1,
      assessment: "source-compatible-not-release-approved",
      sourceFileCount: 172,
      sourceByteCount: 1048576,
      migrationTail: {
        name: "000134_stage6_worker_supply_chain_candidate_evidence.sql",
        sha256: artifactDigest("a"),
      },
    },
    receipts: receiptProjections(),
    requiredReceiptCount: 10,
    candidateConsistencyValidated: true,
    environmentEligible: true,
    allRequiredReceiptsReadyForCandidateReview: true,
    eligibleForCandidateEvidenceReview: true,
    assessment: STAGE6_CANDIDATE_RECEIPT_ASSESSMENT,
    validatedAt: "2026-08-01T00:00:00Z",
  };
}

function sha256(bytes: Buffer): string {
  return `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
}

function writeReceipt(receipt = createReceipt()): {
  readonly directory: string;
  readonly path: string;
  readonly digest: string;
} {
  const directory = mkdtempSync(join(tmpdir(), "synara-stage6-candidate-binding-"));
  temporaryDirectories.push(directory);
  const path = join(directory, "candidate-receipt.json");
  const bytes = Buffer.from(`${JSON.stringify(receipt, null, 2)}\n`, "utf8");
  writeFileSync(path, bytes);
  return { directory, path, digest: sha256(bytes) };
}

function expected(path: string, digest: string) {
  return {
    candidateReceiptPath: path,
    expectedCandidateReceiptSha256: digest,
    expectedCandidateId: candidateId,
    expectedSourceCommit: sourceCommit,
    expectedLockfileSha256: lockfileSha256,
    expectedDesktopArtifactSetSha256: desktopArtifactSetSha256,
  };
}

describe("Stage 6 candidate release binding", () => {
  it("returns a structured binding only after validating the exact receipt bytes", () => {
    const fixture = writeReceipt();
    const binding = verifyStage6CandidateReleaseBinding(expected(fixture.path, fixture.digest));
    expect(binding.receipt).toMatchObject({
      path: fixture.path,
      schemaVersion: STAGE6_CANDIDATE_RECEIPT_SCHEMA,
    });
    expect(binding).toMatchObject({
      candidateId,
      sourceCommit,
      lockfileSha256,
      desktopArtifactSetSha256,
      candidateBundleReceiptSha256: fixture.digest,
    });
    expect(binding.verification).toEqual({
      receiptDigestMatched: true,
      receiptSchemaValidated: true,
      allReceiptProjectionsValidated: true,
      workerSupplyChainReceiptValidated: true,
      candidateIdentityMatched: true,
      candidateEligibilityValidated: true,
    });
    expect(() => assertVerifiedStage6CandidateReleaseBinding(binding)).not.toThrow();
    expect(() => assertVerifiedStage6CandidateReleaseBinding({ ...binding })).toThrow(
      "was not produced by the local receipt verifier",
    );
    expect(Object.isFrozen(binding)).toBe(true);
    expect(JSON.parse(JSON.stringify(binding))).not.toHaveProperty("verified");
  });

  it("accepts environment input and emits JSON plus GitHub outputs", () => {
    const fixture = writeReceipt();
    const githubOutput = join(fixture.directory, "github-output.txt");
    const result = spawnSync(process.execPath, [verifierScript], {
      encoding: "utf8",
      env: {
        ...process.env,
        STAGE6_CANDIDATE_RECEIPT_PATH: fixture.path,
        CANDIDATE_BUNDLE_RECEIPT_SHA256: fixture.digest,
        EXPECTED_CANDIDATE_ID: candidateId,
        EXPECTED_SOURCE_COMMIT: sourceCommit,
        EXPECTED_LOCKFILE_SHA256: lockfileSha256,
        DESKTOP_ARTIFACT_SET_SHA256: desktopArtifactSetSha256,
        GITHUB_OUTPUT: githubOutput,
      },
    });
    expect(result.status).toBe(0);
    expect(JSON.parse(result.stdout)).toMatchObject({
      candidateId,
      candidateBundleReceiptSha256: fixture.digest,
    });
    expect(readFileSync(githubOutput, "utf8")).toContain(
      `candidate_bundle_receipt_sha256=${fixture.digest}\n`,
    );
    expect(readFileSync(githubOutput, "utf8")).toContain("verified=true\n");
  });

  it("rejects all-zero and incorrect expected receipt digests", () => {
    const fixture = writeReceipt();
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(fixture.path, `sha256:${"0".repeat(64)}`)),
    ).toThrow("all-zero");
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(fixture.path, `sha256:${"f".repeat(64)}`)),
    ).toThrow("does not match the expected digest");
  });

  it("rejects a missing or tampered receipt", () => {
    const fixture = writeReceipt();
    expect(() =>
      verifyStage6CandidateReleaseBinding(
        expected(join(fixture.directory, "missing.json"), fixture.digest),
      ),
    ).toThrow("does not exist");
    writeFileSync(fixture.path, `${readFileSync(fixture.path, "utf8")} `);
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(fixture.path, fixture.digest)),
    ).toThrow("does not match the expected digest");
  });

  it("rejects an ineligible candidate receipt", () => {
    const receipt = createReceipt();
    receipt.eligibleForCandidateEvidenceReview = false;
    const fixture = writeReceipt(receipt);
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(fixture.path, fixture.digest)),
    ).toThrow("eligibleForCandidateEvidenceReview must be true");
  });

  it("rejects a missing, ineligible, or forged Worker supply-chain projection", () => {
    for (const mutate of [
      (receipt: Record<string, unknown>) =>
        delete (receipt.receipts as Record<string, unknown>).workerSupplyChain,
      (receipt: Record<string, unknown>) =>
        ((
          (receipt.receipts as Record<string, unknown>).workerSupplyChain as Record<string, unknown>
        ).readyForCandidateReview = false),
      (receipt: Record<string, unknown>) =>
        ((
          (receipt.receipts as Record<string, unknown>).workerSupplyChain as Record<string, unknown>
        ).registryReportSha256 = `sha256:${"0".repeat(64)}`),
    ]) {
      const receipt = createReceipt();
      mutate(receipt);
      const fixture = writeReceipt(receipt);
      expect(() =>
        verifyStage6CandidateReleaseBinding(expected(fixture.path, fixture.digest)),
      ).toThrow();
    }
  });

  it("rejects an empty, malformed, or path-reused projection for every receipt family", () => {
    for (const name of Object.keys(receiptProjections())) {
      const receipt = createReceipt();
      (receipt.receipts as Record<string, unknown>)[name] = {};
      const fixture = writeReceipt(receipt);
      expect(() =>
        verifyStage6CandidateReleaseBinding(expected(fixture.path, fixture.digest)),
      ).toThrow();
    }

    const duplicatePath = createReceipt();
    const projections = duplicatePath.receipts as Record<string, Record<string, unknown>>;
    projections.capacity!.path = projections.internalCost!.path;
    const duplicateFixture = writeReceipt(duplicatePath);
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(duplicateFixture.path, duplicateFixture.digest)),
    ).toThrow("distinct evidence paths");

    const traversal = createReceipt();
    ((traversal.receipts as Record<string, unknown>).internalCost as Record<string, unknown>).path =
      "../internal-cost.json";
    const traversalFixture = writeReceipt(traversal);
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(traversalFixture.path, traversalFixture.digest)),
    ).toThrow("traversal-free");
  });

  it("rejects forged candidate internals and validation chronology", () => {
    const emptyArtifacts = createReceipt();
    (emptyArtifacts.candidate as Record<string, unknown>).artifacts = {};
    const emptyArtifactsFixture = writeReceipt(emptyArtifacts);
    expect(() =>
      verifyStage6CandidateReleaseBinding(
        expected(emptyArtifactsFixture.path, emptyArtifactsFixture.digest),
      ),
    ).toThrow("candidate.artifacts fields");

    const futureReceipt = createReceipt();
    ((futureReceipt.receipts as Record<string, unknown>).internalCost as Record<string, unknown>)[
      "validatedAt"
    ] = "2026-08-02T00:00:00Z";
    const futureFixture = writeReceipt(futureReceipt);
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(futureFixture.path, futureFixture.digest)),
    ).toThrow("must not follow");

    const residencyDrift = createReceipt();
    ((residencyDrift.receipts as Record<string, unknown>).residency as Record<string, unknown>)[
      "allowedRegions"
    ] = ["eu-west-1"];
    const residencyFixture = writeReceipt(residencyDrift);
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(residencyFixture.path, residencyFixture.digest)),
    ).toThrow("does not match the candidate Regions");

    const compatibilityDrift = createReceipt();
    (
      (compatibilityDrift.compatibilityMatrix as Record<string, unknown>).migrationTail as Record<
        string,
        unknown
      >
    ).sha256 = artifactDigest("f");
    const compatibilityFixture = writeReceipt(compatibilityDrift);
    expect(() =>
      verifyStage6CandidateReleaseBinding(
        expected(compatibilityFixture.path, compatibilityFixture.digest),
      ),
    ).toThrow("compatibility matrix migration tail");
  });

  it.each([
    ["schema", (receipt: Record<string, unknown>) => (receipt.schemaVersion = "unsupported")],
    ["assessment", (receipt: Record<string, unknown>) => (receipt.assessment = "ga-passed")],
    [
      "candidate ID",
      (receipt: Record<string, unknown>) =>
        ((receipt.candidate as Record<string, unknown>).candidateId = "v0.6.3-other"),
    ],
    [
      "source commit",
      (receipt: Record<string, unknown>) =>
        ((receipt.candidate as Record<string, unknown>).sourceCommit = "c".repeat(40)),
    ],
    [
      "lockfile digest",
      (receipt: Record<string, unknown>) =>
        ((receipt.candidate as Record<string, unknown>).lockfileSha256 = "c".repeat(64)),
    ],
    [
      "Desktop artifact-set digest",
      (receipt: Record<string, unknown>) =>
        ((receipt.candidate as Record<string, unknown>).desktopArtifactSetSha256 =
          `sha256:${"e".repeat(64)}`),
    ],
  ])("rejects a receipt with mismatched %s", (_label, mutate) => {
    const receipt = createReceipt();
    mutate(receipt);
    const fixture = writeReceipt(receipt);
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(fixture.path, fixture.digest)),
    ).toThrow();
  });

  it("rejects a Desktop projection without a real Enrollment evidence-set digest", () => {
    const receipt = createReceipt();
    const desktop = (receipt.receipts as Record<string, unknown>).desktop as Record<
      string,
      unknown
    >;
    desktop.desktopEnrollmentEvidenceSetSha256 = `sha256:${"0".repeat(64)}`;
    const fixture = writeReceipt(receipt);
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(fixture.path, fixture.digest)),
    ).toThrow("desktop.desktopEnrollmentEvidenceSetSha256");
  });

  it("rejects a symlink and an oversized regular file", () => {
    const fixture = writeReceipt();
    const symlinkPath = join(fixture.directory, "candidate-link.json");
    symlinkSync(fixture.path, symlinkPath);
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(symlinkPath, fixture.digest)),
    ).toThrow("regular non-symlink file");

    const oversizedPath = join(fixture.directory, "oversized.json");
    writeFileSync(oversizedPath, "");
    truncateSync(oversizedPath, STAGE6_CANDIDATE_RECEIPT_MAX_BYTES + 1);
    expect(() =>
      verifyStage6CandidateReleaseBinding(expected(oversizedPath, fixture.digest)),
    ).toThrow("exceeds its bounded size");
  });
});
