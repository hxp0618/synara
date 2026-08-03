#!/usr/bin/env node

import { verifyStage6DesktopArtifactSet } from "./lib/stage6-desktop-artifact-set.ts";

function parseArgs(argv: ReadonlyArray<string>): Map<string, string> {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name?.startsWith("--") || value === undefined) {
      throw new Error(`Invalid Stage 6 Desktop artifact-set argument near ${name ?? "<end>"}.`);
    }
    if (values.has(name))
      throw new Error(`Duplicate Stage 6 Desktop artifact-set argument: ${name}.`);
    values.set(name, value);
  }
  const known = new Set([
    "--assets-dir",
    "--candidate-tag",
    "--source-commit",
    "--lockfile-sha256",
    "--expected-artifact-set-sha256",
  ]);
  for (const name of values.keys()) {
    if (!known.has(name))
      throw new Error(`Unknown Stage 6 Desktop artifact-set argument: ${name}.`);
  }
  return values;
}

const values = parseArgs(process.argv.slice(2));
const required = (name: string): string => {
  const value = values.get(name);
  if (!value) throw new Error(`Missing Stage 6 Desktop artifact-set argument: ${name}.`);
  return value;
};
console.log(
  JSON.stringify(
    verifyStage6DesktopArtifactSet({
      assetsDirectory: required("--assets-dir"),
      candidateTag: required("--candidate-tag"),
      sourceCommit: required("--source-commit"),
      lockfileSha256: required("--lockfile-sha256"),
      expectedArtifactSetSha256: required("--expected-artifact-set-sha256"),
    }),
    null,
    2,
  ),
);
