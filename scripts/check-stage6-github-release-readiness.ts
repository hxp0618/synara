#!/usr/bin/env node
// FILE: check-stage6-github-release-readiness.ts
// Purpose: Prints a read-only Stage 6 projection using secret names and bounded non-secret controls.
// Layer: Stage 6 release CLI

import { collectStage6GitHubReleaseReadiness } from "./lib/stage6-github-release-readiness.ts";

function parseArgs(argv: ReadonlyArray<string>) {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (
      !name ||
      value === undefined ||
      !["--repository", "--branch", "--workflow"].includes(name)
    ) {
      throw new Error(`Invalid Stage 6 GitHub readiness argument near ${name ?? "<end>"}.`);
    }
    if (values.has(name)) throw new Error(`Duplicate Stage 6 GitHub readiness argument ${name}.`);
    values.set(name, value);
  }
  return values;
}

const args = parseArgs(process.argv.slice(2));
const repository = args.get("--repository");
if (!repository) throw new Error("Missing Stage 6 GitHub readiness argument --repository.");

const readiness = collectStage6GitHubReleaseReadiness({
  repository,
  sourceBranch: args.get("--branch") ?? "main",
  workflowPath: args.get("--workflow") ?? ".github/workflows/release.yml",
});
process.stdout.write(`${JSON.stringify(readiness, null, 2)}\n`);
