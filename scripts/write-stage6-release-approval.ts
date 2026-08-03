#!/usr/bin/env node

import { writeStage6ReleaseApproval } from "./lib/stage6-release-approval.ts";
import { verifyStage6CandidateReleaseBinding } from "./lib/stage6-candidate-release-binding.ts";
import { verifyStage6DesktopArtifactSet } from "./lib/stage6-desktop-artifact-set.ts";
import { verifyStage6EnvironmentApproval } from "./lib/stage6-environment-approval.ts";
import { verifyReleaseFinalAssetSet } from "./lib/release-final-asset-set.ts";
import { verifyStage6ReleaseCrossBinding } from "./lib/stage6-release-cross-binding.ts";

function parseArgs(argv: ReadonlyArray<string>): Map<string, string> {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name?.startsWith("--") || value === undefined) {
      throw new Error(`Invalid Stage 6 release approval argument near ${name ?? "<end>"}.`);
    }
    if (values.has(name)) throw new Error(`Duplicate Stage 6 release approval argument: ${name}.`);
    values.set(name, value);
  }
  const known = new Set([
    "--output",
    "--expected-candidate-id",
    "--declared-candidate-id",
    "--expected-source-commit",
    "--declared-source-commit",
    "--expected-build-run-id",
    "--declared-build-run-id",
    "--candidate-bundle-receipt",
    "--candidate-bundle-receipt-sha256",
    "--expected-lockfile-sha256",
    "--desktop-artifact-set-sha256",
    "--raw-desktop-assets-dir",
    "--final-assets-dir",
    "--expected-final-asset-set-sha256",
    "--release-version",
    "--update-channel",
    "--repository",
    "--workflow-ref",
    "--environment-response",
    "--approval-history-response",
    "--workflow-run-response",
    "--branch-protection-response",
    "--run-attempt",
    "--actor",
    "--triggering-actor",
    "--expected-review-comment",
    "--recorded-at",
  ]);
  for (const name of values.keys()) {
    if (!known.has(name)) throw new Error(`Unknown Stage 6 release approval argument: ${name}.`);
  }
  return values;
}

const values = parseArgs(process.argv.slice(2));
const required = (name: string): string => {
  const value = values.get(name);
  if (!value) throw new Error(`Missing Stage 6 release approval argument: ${name}.`);
  return value;
};
const runAttemptText = required("--run-attempt");
if (!/^[1-9][0-9]{0,9}$/.test(runAttemptText)) {
  throw new Error("Stage 6 workflow run attempt must be a positive decimal integer.");
}
const candidateBinding = verifyStage6CandidateReleaseBinding({
  candidateReceiptPath: required("--candidate-bundle-receipt"),
  expectedCandidateReceiptSha256: required("--candidate-bundle-receipt-sha256"),
  expectedCandidateId: required("--expected-candidate-id"),
  expectedSourceCommit: required("--expected-source-commit"),
  expectedLockfileSha256: required("--expected-lockfile-sha256"),
  expectedDesktopArtifactSetSha256: required("--desktop-artifact-set-sha256"),
});
const finalAssetSet = verifyReleaseFinalAssetSet({
  assetsDirectory: required("--final-assets-dir"),
  sourceTag: required("--expected-candidate-id"),
  sourceCommit: required("--expected-source-commit"),
  lockfileSha256: required("--expected-lockfile-sha256"),
  version: required("--release-version"),
  updateChannel: required("--update-channel"),
  expectedFinalAssetSetSha256: required("--expected-final-asset-set-sha256"),
});
const rawDesktopArtifactSet = verifyStage6DesktopArtifactSet({
  assetsDirectory: required("--raw-desktop-assets-dir"),
  candidateTag: required("--expected-candidate-id"),
  sourceCommit: required("--expected-source-commit"),
  lockfileSha256: required("--expected-lockfile-sha256"),
  expectedArtifactSetSha256: required("--desktop-artifact-set-sha256"),
});
const releaseCrossBinding = verifyStage6ReleaseCrossBinding(rawDesktopArtifactSet, finalAssetSet);
const environmentApproval = verifyStage6EnvironmentApproval({
  environmentResponsePath: required("--environment-response"),
  approvalHistoryResponsePath: required("--approval-history-response"),
  workflowRunResponsePath: required("--workflow-run-response"),
  branchProtectionResponsePath: required("--branch-protection-response"),
  repository: required("--repository"),
  environmentName: "stage6-enterprise-ga",
  runId: required("--expected-build-run-id"),
  runAttempt: Number(runAttemptText),
  expectedSourceCommit: required("--expected-source-commit"),
  actor: required("--actor"),
  triggeringActor: required("--triggering-actor"),
  expectedReviewComment: required("--expected-review-comment"),
});
const result = writeStage6ReleaseApproval(required("--output"), {
  expectedCandidateId: required("--expected-candidate-id"),
  declaredCandidateId: required("--declared-candidate-id"),
  expectedSourceCommit: required("--expected-source-commit"),
  declaredSourceCommit: required("--declared-source-commit"),
  expectedBuildRunId: required("--expected-build-run-id"),
  declaredBuildRunId: required("--declared-build-run-id"),
  candidateBinding,
  releaseCrossBinding,
  environmentApproval,
  repository: required("--repository"),
  workflowRef: required("--workflow-ref"),
  recordedAt: required("--recorded-at"),
});
console.log(`Wrote ${result.path} and ${result.sha256Path}.`);
