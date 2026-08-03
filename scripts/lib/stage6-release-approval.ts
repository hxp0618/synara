import { createHash } from "node:crypto";
import { mkdirSync, unlinkSync, writeFileSync } from "node:fs";
import { basename, dirname, resolve } from "node:path";

import {
  type VerifiedStage6CandidateReleaseBinding,
  assertVerifiedStage6CandidateReleaseBinding,
} from "./stage6-candidate-release-binding.ts";
import {
  type VerifiedStage6EnvironmentApproval,
  assertVerifiedStage6EnvironmentApproval,
} from "./stage6-environment-approval.ts";
import {
  type VerifiedStage6ReleaseCrossBinding,
  assertVerifiedStage6ReleaseCrossBinding,
} from "./stage6-release-cross-binding.ts";

const COMMIT_PATTERN = /^[0-9a-f]{40}$/;
const SHA256_PATTERN = /^sha256:[0-9a-f]{64}$/;
const RUN_ID_PATTERN = /^[1-9][0-9]{0,19}$/;
const IDENTIFIER_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._+/-]{1,199}$/;
const REPOSITORY_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;

export const STAGE6_RELEASE_APPROVAL_SCHEMA = "synara.stage6-release-approval.v4";
export const STAGE6_RELEASE_APPROVAL_ASSESSMENT =
  "protected-environment-gate-recorded-not-ga-approved";

export interface Stage6ReleaseApprovalInput {
  readonly expectedCandidateId: string;
  readonly declaredCandidateId: string;
  readonly expectedSourceCommit: string;
  readonly declaredSourceCommit: string;
  readonly expectedBuildRunId: string;
  readonly declaredBuildRunId: string;
  readonly candidateBinding: VerifiedStage6CandidateReleaseBinding;
  readonly releaseCrossBinding: VerifiedStage6ReleaseCrossBinding;
  readonly environmentApproval: VerifiedStage6EnvironmentApproval;
  readonly repository: string;
  readonly workflowRef: string;
  readonly recordedAt: string;
}

export interface Stage6ReleaseApprovalRecord {
  readonly schemaVersion: typeof STAGE6_RELEASE_APPROVAL_SCHEMA;
  readonly candidate: {
    readonly candidateId: string;
    readonly sourceCommit: string;
    readonly lockfileSha256: string;
    readonly candidateBundleReceiptSha256: string;
    readonly desktopArtifactSetSha256: string;
    readonly finalAssetSetSha256: string;
    readonly rawFinalCrossBindingSha256: string;
  };
  readonly build: {
    readonly runId: string;
    readonly runAttempt: 1;
    readonly repository: string;
    readonly workflowRef: string;
    readonly sourceBranch: string;
    readonly actor: string;
    readonly triggeringActor: string;
  };
  readonly approval: {
    readonly environment: "stage6-enterprise-ga";
    readonly environmentId: number;
    readonly configuredReviewers: VerifiedStage6EnvironmentApproval["environment"]["configuredReviewers"];
    readonly preventSelfReview: true;
    readonly administratorBypassDisabled: true;
    readonly protectedBranchesOnly: true;
    readonly actualReviewer: VerifiedStage6EnvironmentApproval["review"]["reviewer"];
    readonly commentSha256: string;
    readonly environmentConfigurationSha256: string;
    readonly reviewSha256: string;
    readonly approvalEvidenceSha256: string;
    readonly recordedAt: string;
  };
  readonly exactCandidatePublicationAuthorized: true;
  readonly assessment: typeof STAGE6_RELEASE_APPROVAL_ASSESSMENT;
}

function requirePattern(value: string, pattern: RegExp, label: string): string {
  const normalized = value.trim();
  if (!pattern.test(normalized)) throw new Error(`${label} is invalid.`);
  return normalized;
}

export function createStage6ReleaseApproval(
  input: Stage6ReleaseApprovalInput,
): Stage6ReleaseApprovalRecord {
  assertVerifiedStage6CandidateReleaseBinding(input.candidateBinding);
  assertVerifiedStage6ReleaseCrossBinding(input.releaseCrossBinding);
  assertVerifiedStage6EnvironmentApproval(input.environmentApproval);
  const expectedCandidateId = requirePattern(
    input.expectedCandidateId,
    IDENTIFIER_PATTERN,
    "Expected candidate ID",
  );
  const declaredCandidateId = requirePattern(
    input.declaredCandidateId,
    IDENTIFIER_PATTERN,
    "Declared candidate ID",
  );
  if (declaredCandidateId !== expectedCandidateId) {
    throw new Error("Declared candidate ID does not match the workflow release tag.");
  }
  if (input.candidateBinding.candidateId !== expectedCandidateId) {
    throw new Error("Verified candidate receipt does not match the workflow release tag.");
  }
  const expectedSourceCommit = requirePattern(
    input.expectedSourceCommit,
    COMMIT_PATTERN,
    "Expected source commit",
  );
  const declaredSourceCommit = requirePattern(
    input.declaredSourceCommit,
    COMMIT_PATTERN,
    "Declared source commit",
  );
  if (declaredSourceCommit !== expectedSourceCommit) {
    throw new Error("Declared source commit does not match the artifact build.");
  }
  if (input.candidateBinding.sourceCommit !== expectedSourceCommit) {
    throw new Error("Verified candidate receipt does not match the artifact build source.");
  }
  if (
    input.releaseCrossBinding.source.tag !== expectedCandidateId ||
    input.releaseCrossBinding.source.commit !== expectedSourceCommit ||
    input.releaseCrossBinding.source.lockfileSha256 !== input.candidateBinding.lockfileSha256 ||
    input.releaseCrossBinding.rawDesktopArtifactSetSha256 !==
      input.candidateBinding.desktopArtifactSetSha256
  ) {
    throw new Error(
      "Verified raw-to-final release binding does not match the candidate receipt and source.",
    );
  }
  const expectedBuildRunId = requirePattern(
    input.expectedBuildRunId,
    RUN_ID_PATTERN,
    "Expected build run ID",
  );
  const declaredBuildRunId = requirePattern(
    input.declaredBuildRunId,
    RUN_ID_PATTERN,
    "Declared build run ID",
  );
  if (declaredBuildRunId !== expectedBuildRunId) {
    throw new Error("Declared build run ID does not match this workflow run.");
  }
  if (input.environmentApproval.workflow.runId !== expectedBuildRunId) {
    throw new Error("Verified GitHub environment approval does not match this workflow run.");
  }
  if (input.environmentApproval.workflow.sourceCommit !== expectedSourceCommit) {
    throw new Error("Verified GitHub environment approval does not match this source commit.");
  }
  const candidateBundleReceiptSha256 = requirePattern(
    input.candidateBinding.candidateBundleReceiptSha256,
    SHA256_PATTERN,
    "Verified candidate bundle receipt SHA-256",
  );
  const desktopArtifactSetSha256 = requirePattern(
    input.candidateBinding.desktopArtifactSetSha256,
    SHA256_PATTERN,
    "Verified Desktop artifact-set SHA-256",
  );
  const repository = requirePattern(input.repository, REPOSITORY_PATTERN, "Repository");
  const workflowRef = input.workflowRef.trim();
  const sourceBranch = input.environmentApproval.workflow.sourceBranch;
  if (workflowRef !== `${repository}/.github/workflows/release.yml@refs/heads/${sourceBranch}`) {
    throw new Error(
      "Workflow ref does not identify this protected source branch release workflow.",
    );
  }
  if (
    new URL(input.environmentApproval.environment.apiUrl).pathname !==
    `/repos/${repository}/environments/stage6-enterprise-ga`
  ) {
    throw new Error("Verified GitHub environment approval belongs to a different repository.");
  }
  const recordedDate = new Date(input.recordedAt);
  if (!input.recordedAt.endsWith("Z") || Number.isNaN(recordedDate.getTime())) {
    throw new Error("Approval record time must be an RFC3339 UTC timestamp ending in Z.");
  }
  return {
    schemaVersion: STAGE6_RELEASE_APPROVAL_SCHEMA,
    candidate: {
      candidateId: expectedCandidateId,
      sourceCommit: expectedSourceCommit,
      lockfileSha256: input.candidateBinding.lockfileSha256,
      candidateBundleReceiptSha256,
      desktopArtifactSetSha256,
      finalAssetSetSha256: input.releaseCrossBinding.finalAssetSetSha256,
      rawFinalCrossBindingSha256: input.releaseCrossBinding.crossBindingSha256,
    },
    build: {
      runId: expectedBuildRunId,
      runAttempt: input.environmentApproval.workflow.runAttempt,
      repository,
      workflowRef,
      sourceBranch,
      actor: input.environmentApproval.workflow.actor,
      triggeringActor: input.environmentApproval.workflow.triggeringActor,
    },
    approval: {
      environment: "stage6-enterprise-ga",
      environmentId: input.environmentApproval.environment.id,
      configuredReviewers: input.environmentApproval.environment.configuredReviewers,
      preventSelfReview: true,
      administratorBypassDisabled: true,
      protectedBranchesOnly: true,
      actualReviewer: input.environmentApproval.review.reviewer,
      commentSha256: input.environmentApproval.review.commentSha256,
      environmentConfigurationSha256: input.environmentApproval.environment.configurationSha256,
      reviewSha256: input.environmentApproval.review.reviewSha256,
      approvalEvidenceSha256: input.environmentApproval.approvalEvidenceSha256,
      recordedAt: recordedDate.toISOString(),
    },
    exactCandidatePublicationAuthorized: true,
    assessment: STAGE6_RELEASE_APPROVAL_ASSESSMENT,
  };
}

export function encodeStage6ReleaseApproval(record: Stage6ReleaseApprovalRecord): Buffer {
  return Buffer.from(`${JSON.stringify(record, null, 2)}\n`, "utf8");
}

export function writeStage6ReleaseApproval(
  outputPath: string,
  input: Stage6ReleaseApprovalInput,
): {
  readonly path: string;
  readonly sha256Path: string;
  readonly sha256: string;
} {
  const absoluteOutput = resolve(outputPath);
  const record = createStage6ReleaseApproval(input);
  const encoded = encodeStage6ReleaseApproval(record);
  const sha256 = createHash("sha256").update(encoded).digest("hex");
  const sha256Path = `${absoluteOutput}.sha256`;
  mkdirSync(dirname(absoluteOutput), { recursive: true });
  writeFileSync(absoluteOutput, encoded, { flag: "wx", mode: 0o600 });
  try {
    writeFileSync(sha256Path, `${sha256}  ${basename(absoluteOutput)}\n`, {
      encoding: "utf8",
      flag: "wx",
      mode: 0o600,
    });
  } catch (error) {
    unlinkSync(absoluteOutput);
    throw error;
  }
  return { path: absoluteOutput, sha256Path, sha256 };
}
