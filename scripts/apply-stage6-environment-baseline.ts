#!/usr/bin/env node

import {
  applyStage6EnvironmentBaseline,
  createStage6EnvironmentBaselinePlan,
  hashStage6EnvironmentBaselinePlan,
} from "./lib/stage6-environment-baseline.ts";
import type { Stage6EnvironmentReviewer } from "./lib/stage6-protected-environment-apply.ts";

const values = new Map<string, string>();
const reviewers: Stage6EnvironmentReviewer[] = [];
let apply = false;
const argv = process.argv.slice(2);
for (let index = 0; index < argv.length; ) {
  const name = argv[index];
  if (name === "--apply") {
    if (apply) throw new Error("Duplicate Stage 6 Environment baseline argument --apply.");
    apply = true;
    index += 1;
    continue;
  }
  const value = argv[index + 1];
  if (!name || value === undefined) {
    throw new Error(`Invalid Stage 6 Environment baseline argument near ${name ?? "<end>"}.`);
  }
  if (name === "--reviewer") {
    const match = /^(User|Team):([1-9][0-9]{0,15})$/.exec(value);
    if (!match) throw new Error(`Invalid Stage 6 Environment reviewer ${value}.`);
    const id = Number(match[2]);
    if (!Number.isSafeInteger(id))
      throw new Error(`Invalid Stage 6 Environment reviewer ${value}.`);
    reviewers.push({ type: match[1] as "User" | "Team", id });
  } else if (
    ["--repository", "--branch", "--expected-head", "--confirm-sha256"].includes(name) &&
    !values.has(name)
  ) {
    values.set(name, value);
  } else {
    throw new Error(`Invalid Stage 6 Environment baseline argument ${name}.`);
  }
  index += 2;
}

const required = (name: string): string => {
  const value = values.get(name);
  if (!value) throw new Error(`Missing Stage 6 Environment baseline argument ${name}.`);
  return value;
};
const plan = createStage6EnvironmentBaselinePlan({
  repository: required("--repository"),
  sourceBranch: required("--branch"),
  expectedHeadCommit: required("--expected-head"),
  reviewers,
});
const planSha256 = hashStage6EnvironmentBaselinePlan(plan);

if (!apply) {
  if (values.has("--confirm-sha256")) {
    throw new Error("--confirm-sha256 is only valid together with --apply.");
  }
  process.stdout.write(`${JSON.stringify({ plan, planSha256 }, null, 2)}\n`);
} else {
  const receipt = applyStage6EnvironmentBaseline({
    plan,
    confirmationSha256: required("--confirm-sha256"),
  });
  process.stdout.write(`${JSON.stringify(receipt, null, 2)}\n`);
}
