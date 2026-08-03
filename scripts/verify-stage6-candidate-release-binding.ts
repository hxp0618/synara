#!/usr/bin/env node

import { appendFileSync } from "node:fs";

import { serializeReleaseGithubOutput } from "./lib/release-github-output.ts";
import { verifyStage6CandidateReleaseBinding } from "./lib/stage6-candidate-release-binding.ts";

const ARGUMENTS = {
  "--receipt": "STAGE6_CANDIDATE_RECEIPT_PATH",
  "--expected-receipt-sha256": "CANDIDATE_BUNDLE_RECEIPT_SHA256",
  "--expected-candidate-id": "EXPECTED_CANDIDATE_ID",
  "--expected-source-commit": "EXPECTED_SOURCE_COMMIT",
  "--expected-lockfile-sha256": "EXPECTED_LOCKFILE_SHA256",
  "--expected-desktop-artifact-set-sha256": "DESKTOP_ARTIFACT_SET_SHA256",
} as const;

function parseArgs(argv: ReadonlyArray<string>): Map<string, string> {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name?.startsWith("--") || value === undefined) {
      throw new Error(
        `Invalid Stage 6 candidate release-binding argument near ${name ?? "<end>"}.`,
      );
    }
    if (!(name in ARGUMENTS)) {
      throw new Error(`Unknown Stage 6 candidate release-binding argument: ${name}.`);
    }
    if (values.has(name)) {
      throw new Error(`Duplicate Stage 6 candidate release-binding argument: ${name}.`);
    }
    values.set(name, value);
  }
  return values;
}

const values = parseArgs(process.argv.slice(2));
const required = (name: keyof typeof ARGUMENTS): string => {
  const value = values.get(name) ?? process.env[ARGUMENTS[name]];
  if (!value) {
    throw new Error(
      `Missing Stage 6 candidate release-binding argument ${name} or environment variable ${ARGUMENTS[name]}.`,
    );
  }
  return value;
};

const binding = verifyStage6CandidateReleaseBinding({
  candidateReceiptPath: required("--receipt"),
  expectedCandidateReceiptSha256: required("--expected-receipt-sha256"),
  expectedCandidateId: required("--expected-candidate-id"),
  expectedSourceCommit: required("--expected-source-commit"),
  expectedLockfileSha256: required("--expected-lockfile-sha256"),
  expectedDesktopArtifactSetSha256: required("--expected-desktop-artifact-set-sha256"),
});

const githubOutput = process.env.GITHUB_OUTPUT;
if (githubOutput) {
  appendFileSync(
    githubOutput,
    serializeReleaseGithubOutput({
      candidate_bundle_receipt_sha256: binding.candidateBundleReceiptSha256,
      candidate_id: binding.candidateId,
      source_commit: binding.sourceCommit,
      lockfile_sha256: binding.lockfileSha256,
      desktop_artifact_set_sha256: binding.desktopArtifactSetSha256,
      verified: "true",
    }),
  );
}
console.log(JSON.stringify(binding, null, 2));
