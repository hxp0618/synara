import { createHash } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";

import {
  STAGE6_CANDIDATE_RECEIPT_ASSESSMENT,
  STAGE6_CANDIDATE_RECEIPT_SCHEMA,
  verifyStage6CandidateReleaseBinding,
} from "./stage6-candidate-release-binding.ts";
import {
  computeStage6DesktopArtifactSetSha256,
  verifyStage6DesktopArtifactSet,
} from "./stage6-desktop-artifact-set.ts";
import { verifyStage6EnvironmentApproval } from "./stage6-environment-approval.ts";
import {
  verifyReleaseFinalAssetSet,
  writeReleaseFinalAssetSet,
} from "./release-final-asset-set.ts";
import {
  type Stage6ReleaseApprovalInput,
  STAGE6_RELEASE_APPROVAL_ASSESSMENT,
  createStage6ReleaseApproval,
  writeStage6ReleaseApproval,
} from "./stage6-release-approval.ts";
import { verifyStage6ReleaseCrossBinding } from "./stage6-release-cross-binding.ts";

const candidateId = "v0.6.3-stage6.1";
const version = candidateId.slice(1);
const sourceCommit = "a".repeat(40);
const lockfileSha256 = "b".repeat(64);
const runId = "123456789";
const temporaryDirectories: Array<string> = [];
const artifactDigest = (character: string): string => `sha256:${character.repeat(64)}`;

function candidateReceiptProjection(
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

function candidateReceiptProjections(artifactSetSha256: string): Record<string, unknown> {
  return {
    internalCost: candidateReceiptProjection(
      "internalCost",
      "synara.stage6-internal-cost-evidence-validation.v1",
      "evidence-validated-not-internal-cost-approved",
    ),
    capacity: candidateReceiptProjection(
      "capacity",
      "synara.capacity-soak-evidence-receipt.v1",
      "evidence-validated-not-capacity-passed",
    ),
    desktop: candidateReceiptProjection(
      "desktop",
      "synara.stage6-desktop-native-acceptance-validation.v1",
      "evidence-validated-not-desktop-ga-passed",
      {
        desktopArtifactSetSha256: artifactSetSha256,
        desktopEnrollmentEvidenceSetSha256: artifactDigest("e"),
      },
    ),
    incident: candidateReceiptProjection(
      "incident",
      "synara.incident-communication-exercise-evidence-receipt.v2",
      "evidence-validated-not-operations-ready",
    ),
    operations: candidateReceiptProjection(
      "operations",
      "synara.stage6-operations-browser-exercise-validation.v2",
      "evidence-validated-not-operations-passed",
    ),
    penetration: candidateReceiptProjection(
      "penetration",
      "synara.third-party-penetration-evidence-receipt.v1",
      "evidence-validated-not-penetration-passed",
    ),
    recovery: candidateReceiptProjection(
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
    residency: candidateReceiptProjection(
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
    slo: candidateReceiptProjection(
      "slo",
      "synara.slo-window-evidence-receipt.v1",
      "evidence-validated-not-slo-passed",
    ),
    workerSupplyChain: candidateReceiptProjection(
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

const targets = {
  "linux-x64": {
    platform: "linux",
    arch: "x64",
    target: "AppImage",
    artifacts: [`Synara-${version}-x64.AppImage`],
  },
  "macos-arm64": {
    platform: "mac",
    arch: "arm64",
    target: "dmg",
    artifacts: [`Synara-${version}-arm64.dmg`, `Synara-${version}-arm64-mac.zip`],
  },
  "macos-x64": {
    platform: "mac",
    arch: "x64",
    target: "dmg",
    artifacts: [`Synara-${version}-x64.dmg`, `Synara-${version}-x64-mac.zip`],
  },
  "windows-x64": {
    platform: "win",
    arch: "x64",
    target: "nsis",
    artifacts: [`Synara-Setup-${version}.exe`, `Synara-Setup-${version}.exe.blockmap`],
  },
} as const;

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    rmSync(directory, { recursive: true, force: true });
  }
});

function sha256(value: string | Buffer): string {
  return createHash("sha256").update(value).digest("hex");
}

function payloadBytes(fileName: string): string {
  return `${fileName}-bytes`;
}

function updateFileEntry(fileName: string, blockMapSize?: number): string {
  const bytes = payloadBytes(fileName);
  return [
    `  - url: ${fileName}`,
    `    sha512: ${createHash("sha512").update(bytes).digest("base64")}`,
    `    size: ${Buffer.byteLength(bytes)}`,
    ...(blockMapSize === undefined ? [] : [`    blockMapSize: ${blockMapSize}`]),
  ].join("\n");
}

function writeUpdaterManifests(finalAssetsDirectory: string): void {
  const writePair = (defaultName: string, channelName: string, bytes: string): void => {
    writeFileSync(join(finalAssetsDirectory, defaultName), bytes);
    writeFileSync(join(finalAssetsDirectory, channelName), bytes);
  };
  const macArmZip = `Synara-${version}-arm64-mac.zip`;
  const macX64Zip = `Synara-${version}-x64-mac.zip`;
  writePair(
    "latest-mac.yml",
    "synara-mac.yml",
    `version: ${version}\nfiles:\n${updateFileEntry(macArmZip)}\n${updateFileEntry(macX64Zip)}\nreleaseDate: '2026-07-31T00:00:00.000Z'\n`,
  );
  const windowsInstaller = `Synara-Setup-${version}.exe`;
  const windowsBlockmap = `${windowsInstaller}.blockmap`;
  writePair(
    "latest.yml",
    "synara.yml",
    `version: ${version}\nfiles:\n${updateFileEntry(windowsInstaller, Buffer.byteLength(payloadBytes(windowsBlockmap)))}\npath: ${windowsInstaller}\nsha512: ${createHash("sha512").update(payloadBytes(windowsInstaller)).digest("base64")}\nreleaseDate: '2026-07-31T00:00:00.000Z'\n`,
  );
  const linuxInstaller = `Synara-${version}-x64.AppImage`;
  writePair(
    "latest-linux.yml",
    "synara-linux.yml",
    `version: ${version}\nfiles:\n${updateFileEntry(linuxInstaller)}\npath: ${linuxInstaller}\nsha512: ${createHash("sha512").update(payloadBytes(linuxInstaller)).digest("base64")}\nreleaseDate: '2026-07-31T00:00:00.000Z'\n`,
  );
}

function createReleaseBindings(directory: string, mutateFinal?: (directory: string) => void) {
  const rawAssetsDirectory = join(directory, "raw-assets");
  const finalAssetsDirectory = join(directory, "final-assets");
  mkdirSync(rawAssetsDirectory);
  mkdirSync(finalAssetsDirectory);
  const evidenceDigests = {} as Record<
    keyof typeof targets,
    {
      provenance: string;
      attestationBundle: string;
      artifactAttestation: string;
    }
  >;

  for (const [targetId, definition] of Object.entries(targets) as Array<
    [keyof typeof targets, (typeof targets)[keyof typeof targets]]
  >) {
    const artifacts = definition.artifacts.map((fileName) => {
      const bytes = payloadBytes(fileName);
      writeFileSync(join(rawAssetsDirectory, fileName), bytes);
      writeFileSync(join(finalAssetsDirectory, fileName), bytes);
      return {
        fileName,
        size: Buffer.byteLength(bytes),
        sha256: sha256(bytes),
      };
    });
    const provenance = {
      schemaVersion: 1,
      publication: true,
      platform: definition.platform,
      arch: definition.arch,
      target: definition.target,
      version,
      source: { commit: sourceCommit, tag: candidateId, lockfileSha256 },
      signing: { status: "fixture" },
      artifacts,
    };
    const provenanceFile = `artifact-${definition.platform}-${definition.arch}.provenance.json`;
    const provenanceBytes = `${JSON.stringify(provenance, null, 2)}\n`;
    const attestationFile = `artifact-${definition.platform}-${definition.arch}.attestation.json`;
    const verificationFile = `artifact-${definition.platform}-${definition.arch}.attestation-verification.json`;
    const attestationBytes = `${targetId}-attestation-bundle`;
    const verificationBytes = `${targetId}-attestation-verification`;
    for (const root of [rawAssetsDirectory, finalAssetsDirectory]) {
      writeFileSync(join(root, provenanceFile), provenanceBytes);
      writeFileSync(join(root, attestationFile), attestationBytes);
      writeFileSync(join(root, verificationFile), verificationBytes);
    }
    evidenceDigests[targetId] = {
      provenance: `sha256:${sha256(provenanceBytes)}`,
      attestationBundle: `sha256:${sha256(attestationBytes)}`,
      artifactAttestation: `sha256:${sha256(verificationBytes)}`,
    };
  }
  mutateFinal?.(finalAssetsDirectory);
  writeUpdaterManifests(finalAssetsDirectory);
  const desktopArtifactSetSha256 = computeStage6DesktopArtifactSetSha256(evidenceDigests);
  const raw = verifyStage6DesktopArtifactSet({
    assetsDirectory: rawAssetsDirectory,
    candidateTag: candidateId,
    sourceCommit,
    lockfileSha256,
    expectedArtifactSetSha256: desktopArtifactSetSha256,
  });
  const identity = {
    assetsDirectory: finalAssetsDirectory,
    sourceTag: candidateId,
    sourceCommit,
    lockfileSha256,
    version,
    updateChannel: "synara",
  } as const;
  const finalManifest = writeReleaseFinalAssetSet(identity);
  const finalAssets = verifyReleaseFinalAssetSet({
    ...identity,
    expectedFinalAssetSetSha256: finalManifest.manifest.finalAssetSetSha256,
  });
  return {
    desktopArtifactSetSha256,
    raw,
    finalAssets,
    releaseCrossBinding: verifyStage6ReleaseCrossBinding(raw, finalAssets),
  };
}

function createEnvironmentApproval(directory: string) {
  const environmentResponsePath = join(directory, "environment.json");
  const approvalHistoryResponsePath = join(directory, "approvals.json");
  const workflowRunResponsePath = join(directory, "workflow-run.json");
  const branchProtectionResponsePath = join(directory, "branch-protection.json");
  writeFileSync(
    environmentResponsePath,
    `${JSON.stringify({
      id: 987,
      name: "stage6-enterprise-ga",
      url: "https://api.github.com/repos/synara-ai/synara/environments/stage6-enterprise-ga",
      html_url:
        "https://github.com/synara-ai/synara/deployments/activity_log?environments_filter=stage6-enterprise-ga",
      can_admins_bypass: false,
      deployment_branch_policy: {
        protected_branches: true,
        custom_branch_policies: false,
      },
      protection_rules: [
        {
          type: "required_reviewers",
          prevent_self_review: true,
          reviewers: [
            {
              type: "User",
              reviewer: { id: 1, node_id: "USER_release", login: "release" },
            },
            {
              type: "Team",
              reviewer: { id: 2, node_id: "TEAM_security", slug: "security" },
            },
          ],
        },
        { type: "branch_policy" },
      ],
    })}\n`,
  );
  writeFileSync(
    approvalHistoryResponsePath,
    `${JSON.stringify([
      {
        state: "approved",
        comment: `SYNARA_STAGE6_APPROVE ${candidateId} ${runId}`,
        environments: [{ id: 987, name: "stage6-enterprise-ga" }],
        user: {
          id: 3,
          node_id: "USER_security",
          login: "security-reviewer",
          type: "User",
        },
      },
    ])}\n`,
  );
  writeFileSync(
    workflowRunResponsePath,
    `${JSON.stringify({
      id: Number(runId),
      run_attempt: 1,
      event: "workflow_dispatch",
      path: ".github/workflows/release.yml",
      head_sha: sourceCommit,
      head_branch: "main",
      status: "in_progress",
      repository: { full_name: "synara-ai/synara" },
    })}\n`,
  );
  writeFileSync(
    branchProtectionResponsePath,
    `${JSON.stringify({
      enforce_admins: { enabled: true },
      required_pull_request_reviews: {
        dismiss_stale_reviews: true,
        required_approving_review_count: 1,
      },
      required_status_checks: { strict: true, checks: [{ context: "CI" }] },
      allow_force_pushes: { enabled: false },
      allow_deletions: { enabled: false },
    })}\n`,
  );
  return verifyStage6EnvironmentApproval({
    environmentResponsePath,
    approvalHistoryResponsePath,
    workflowRunResponsePath,
    branchProtectionResponsePath,
    repository: "synara-ai/synara",
    environmentName: "stage6-enterprise-ga",
    runId,
    runAttempt: 1,
    expectedSourceCommit: sourceCommit,
    actor: "release-operator",
    triggeringActor: "release-operator",
    expectedReviewComment: `SYNARA_STAGE6_APPROVE ${candidateId} ${runId}`,
  });
}

function createInput(): Stage6ReleaseApprovalInput {
  const directory = mkdtempSync(join(tmpdir(), "synara-stage6-release-approval-input-"));
  temporaryDirectories.push(directory);
  const release = createReleaseBindings(directory);
  const receiptPath = join(directory, "candidate-bundle-receipt.json");
  const receipt = {
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
      desktopArtifactSetSha256: release.desktopArtifactSetSha256,
    },
    releaseEvidence: {
      path: "release-evidence.json",
      sha256: artifactDigest("b"),
      schemaVersion: "synara-stage6-release-evidence-v2",
      assessment: "evidence-collected-not-control-passed",
      generatedAt: "2026-07-31T23:00:00Z",
    },
    receipts: candidateReceiptProjections(release.desktopArtifactSetSha256),
    requiredReceiptCount: 10,
    candidateConsistencyValidated: true,
    environmentEligible: true,
    allRequiredReceiptsReadyForCandidateReview: true,
    eligibleForCandidateEvidenceReview: true,
    assessment: STAGE6_CANDIDATE_RECEIPT_ASSESSMENT,
    validatedAt: "2026-08-01T00:00:00Z",
  };
  const receiptBytes = Buffer.from(`${JSON.stringify(receipt, null, 2)}\n`, "utf8");
  writeFileSync(receiptPath, receiptBytes);
  const candidateBinding = verifyStage6CandidateReleaseBinding({
    candidateReceiptPath: receiptPath,
    expectedCandidateReceiptSha256: `sha256:${sha256(receiptBytes)}`,
    expectedCandidateId: candidateId,
    expectedSourceCommit: sourceCommit,
    expectedLockfileSha256: lockfileSha256,
    expectedDesktopArtifactSetSha256: release.desktopArtifactSetSha256,
  });
  return {
    expectedCandidateId: candidateId,
    declaredCandidateId: candidateId,
    expectedSourceCommit: sourceCommit,
    declaredSourceCommit: sourceCommit,
    expectedBuildRunId: runId,
    declaredBuildRunId: runId,
    candidateBinding,
    releaseCrossBinding: release.releaseCrossBinding,
    environmentApproval: createEnvironmentApproval(directory),
    repository: "synara-ai/synara",
    workflowRef: "synara-ai/synara/.github/workflows/release.yml@refs/heads/main",
    recordedAt: "2026-07-31T12:34:56Z",
  };
}

describe("Stage 6 protected release approval", () => {
  it("rejects final trust or payload bytes that were not in the approved raw candidate", () => {
    const trustRoot = mkdtempSync(join(tmpdir(), "synara-stage6-release-cross-trust-"));
    temporaryDirectories.push(trustRoot);
    expect(() =>
      createReleaseBindings(trustRoot, (finalDirectory) => {
        writeFileSync(
          join(finalDirectory, "artifact-mac-arm64.attestation.json"),
          "different-final-attestation",
        );
      }),
    ).toThrow("does not match the approved raw candidate bytes");

    const payloadRoot = mkdtempSync(join(tmpdir(), "synara-stage6-release-cross-payload-"));
    temporaryDirectories.push(payloadRoot);
    expect(() =>
      createReleaseBindings(payloadRoot, (finalDirectory) => {
        writeFileSync(
          join(finalDirectory, `Synara-${version}-arm64.dmg`),
          "different-final-installer",
        );
      }),
    ).toThrow("does not match the approved raw candidate bytes");
  });

  it("records actual environment approval and the exact raw-to-final candidate binding", () => {
    const input = createInput();
    const record = createStage6ReleaseApproval(input);
    expect(record.candidate).toEqual({
      candidateId,
      sourceCommit,
      lockfileSha256,
      candidateBundleReceiptSha256: input.candidateBinding.candidateBundleReceiptSha256,
      desktopArtifactSetSha256: input.candidateBinding.desktopArtifactSetSha256,
      finalAssetSetSha256: input.releaseCrossBinding.finalAssetSetSha256,
      rawFinalCrossBindingSha256: input.releaseCrossBinding.crossBindingSha256,
    });
    expect(record.build).toMatchObject({
      runId,
      runAttempt: 1,
      sourceBranch: "main",
      actor: "release-operator",
    });
    expect(record.approval).toMatchObject({
      environmentId: 987,
      preventSelfReview: true,
      administratorBypassDisabled: true,
      protectedBranchesOnly: true,
      actualReviewer: { login: "security-reviewer" },
    });
    expect(record.exactCandidatePublicationAuthorized).toBe(true);
    expect(record.assessment).toBe(STAGE6_RELEASE_APPROVAL_ASSESSMENT);
  });

  it.each([
    ["declared candidate ID", { declaredCandidateId: "v0.6.3-other" }],
    ["verified candidate ID", { expectedCandidateId: "v0.6.3-other" }],
    ["declared source commit", { declaredSourceCommit: "c".repeat(40) }],
    ["verified source commit", { expectedSourceCommit: "c".repeat(40) }],
    ["build run ID", { declaredBuildRunId: "987654321" }],
    [
      "workflow source branch",
      { workflowRef: "synara-ai/synara/.github/workflows/release.yml@refs/heads/other" },
    ],
    ["record time", { recordedAt: "not-a-time" }],
  ])("rejects mismatched %s", (_label, change) => {
    expect(() => createStage6ReleaseApproval({ ...createInput(), ...change })).toThrow();
  });

  it("rejects copied verifier outputs", () => {
    const input = createInput();
    expect(() =>
      createStage6ReleaseApproval({
        ...input,
        candidateBinding: {
          ...input.candidateBinding,
        } as typeof input.candidateBinding,
      }),
    ).toThrow("was not produced by the local receipt verifier");
    expect(() =>
      createStage6ReleaseApproval({
        ...input,
        releaseCrossBinding: {
          ...input.releaseCrossBinding,
        } as typeof input.releaseCrossBinding,
      }),
    ).toThrow("was not produced by the local byte verifier");
    expect(() =>
      createStage6ReleaseApproval({
        ...input,
        environmentApproval: {
          ...input.environmentApproval,
        } as typeof input.environmentApproval,
      }),
    ).toThrow("was not produced by the GitHub evidence verifier");
  });

  it("writes immutable JSON and a digest sidecar", () => {
    const directory = mkdtempSync(join(tmpdir(), "synara-stage6-release-approval-"));
    temporaryDirectories.push(directory);
    const output = join(directory, "stage6-enterprise-ga-approval.json");
    const input = createInput();
    const result = writeStage6ReleaseApproval(output, input);
    expect(JSON.parse(readFileSync(output, "utf8")).assessment).toBe(
      STAGE6_RELEASE_APPROVAL_ASSESSMENT,
    );
    expect(readFileSync(result.sha256Path, "utf8")).toBe(
      `${result.sha256}  stage6-enterprise-ga-approval.json\n`,
    );
    expect(statSync(output).mode & 0o777).toBe(0o600);
    expect(statSync(result.sha256Path).mode & 0o777).toBe(0o600);
    expect(() => writeStage6ReleaseApproval(output, input)).toThrow();
  });

  it("removes a partial approval record when the digest sidecar cannot be created", () => {
    const directory = mkdtempSync(join(tmpdir(), "synara-stage6-release-approval-rollback-"));
    temporaryDirectories.push(directory);
    const output = join(directory, "stage6-enterprise-ga-approval.json");
    const sha256Path = `${output}.sha256`;
    writeFileSync(sha256Path, "retained sidecar\n", { mode: 0o600 });

    expect(() => writeStage6ReleaseApproval(output, createInput())).toThrow();
    expect(existsSync(output)).toBe(false);
    expect(readFileSync(sha256Path, "utf8")).toBe("retained sidecar\n");
  });
});
