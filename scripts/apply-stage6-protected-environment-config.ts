#!/usr/bin/env node

import { lstatSync, realpathSync } from "node:fs";
import { basename, dirname, join, resolve } from "node:path";

import {
  type Stage6EnvironmentReviewer,
  applyStage6ProtectedEnvironmentConfiguration,
  writeStage6ProtectedEnvironmentApplicationReceipt,
} from "./lib/stage6-protected-environment-apply.ts";

const SINGLE_ARGUMENTS = new Set(["--configuration", "--repository", "--output"]);

function parseArgs(argv: ReadonlyArray<string>): {
  readonly values: ReadonlyMap<string, string>;
  readonly reviewers: ReadonlyArray<Stage6EnvironmentReviewer>;
} {
  const values = new Map<string, string>();
  const reviewers: Stage6EnvironmentReviewer[] = [];
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name || value === undefined) {
      throw new Error(
        `Invalid protected environment application argument near ${name ?? "<end>"}.`,
      );
    }
    if (name === "--reviewer") {
      const match = /^(User|Team):([1-9][0-9]{0,15})$/.exec(value);
      if (!match) throw new Error(`Invalid Stage 6 reviewer ${value}.`);
      const id = Number(match[2]);
      if (!Number.isSafeInteger(id)) throw new Error(`Invalid Stage 6 reviewer ${value}.`);
      reviewers.push({ type: match[1] as "User" | "Team", id });
      continue;
    }
    if (!SINGLE_ARGUMENTS.has(name) || values.has(name)) {
      throw new Error(`Invalid protected environment application argument ${name}.`);
    }
    values.set(name, value);
  }
  return { values, reviewers };
}

const parsed = parseArgs(process.argv.slice(2));
const required = (name: string): string => {
  const value = parsed.values.get(name);
  if (!value) throw new Error(`Missing protected environment application argument ${name}.`);
  return value;
};

const requestedOutputPath = resolve(required("--output"));
const outputParent = realpathSync(dirname(requestedOutputPath));
const outputParentEntry = lstatSync(outputParent);
if (!outputParentEntry.isDirectory() || outputParentEntry.isSymbolicLink()) {
  throw new Error("Stage 6 environment application output parent must be a real directory.");
}
const outputPath = join(outputParent, basename(requestedOutputPath));
for (const path of [outputPath, `${outputPath}.sha256`]) {
  try {
    lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") continue;
    throw error;
  }
  throw new Error(`Stage 6 environment application output already exists: ${path}`);
}

const receipt = applyStage6ProtectedEnvironmentConfiguration({
  configurationPath: required("--configuration"),
  repository: required("--repository"),
  reviewers: parsed.reviewers,
});
const written = writeStage6ProtectedEnvironmentApplicationReceipt(outputPath, receipt);
console.log(
  JSON.stringify(
    {
      path: written.path,
      sha256Path: written.sha256Path,
      sha256: written.sha256,
      assessment: receipt.assessment,
    },
    null,
    2,
  ),
);
