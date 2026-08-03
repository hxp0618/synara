#!/usr/bin/env node

import { writeStage6ProtectedEnvironmentConfig } from "./lib/stage6-protected-environment-config.ts";

const ARGUMENTS = new Set([
  "--receipt",
  "--candidate-id",
  "--source-commit",
  "--build-run-id",
  "--output",
]);

function parseArgs(argv: ReadonlyArray<string>): Map<string, string> {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name || !ARGUMENTS.has(name) || value === undefined || values.has(name)) {
      throw new Error(
        `Invalid protected environment configuration argument near ${name ?? "<end>"}.`,
      );
    }
    values.set(name, value);
  }
  return values;
}

const values = parseArgs(process.argv.slice(2));
const required = (name: string): string => {
  const value = values.get(name);
  if (!value) throw new Error(`Missing protected environment configuration argument ${name}.`);
  return value;
};

const written = writeStage6ProtectedEnvironmentConfig(required("--output"), {
  candidateReceiptPath: required("--receipt"),
  expectedCandidateId: required("--candidate-id"),
  expectedSourceCommit: required("--source-commit"),
  buildRunId: required("--build-run-id"),
});
console.log(
  JSON.stringify(
    {
      path: written.path,
      sha256Path: written.sha256Path,
      sha256: written.sha256,
      assessment: "configuration-written-with-private-permissions-not-applied-or-approved",
    },
    null,
    2,
  ),
);
