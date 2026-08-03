#!/usr/bin/env node

import {
  applyStage6SourceBranchProtection,
  createStage6SourceBranchProtectionPlan,
  hashStage6SourceBranchProtectionPlan,
} from "./lib/stage6-source-branch-protection.ts";

const values = new Map<string, string>();
const checks: string[] = [];
let apply = false;
const argv = process.argv.slice(2);
for (let index = 0; index < argv.length; ) {
  const name = argv[index];
  if (name === "--apply") {
    if (apply) throw new Error("Duplicate Stage 6 branch-protection argument --apply.");
    apply = true;
    index += 1;
    continue;
  }
  const value = argv[index + 1];
  if (!name || value === undefined) {
    throw new Error(`Invalid Stage 6 branch-protection argument near ${name ?? "<end>"}.`);
  }
  if (name === "--required-check") {
    checks.push(value);
  } else if (
    ["--repository", "--branch", "--expected-head", "--confirm-sha256"].includes(name) &&
    !values.has(name)
  ) {
    values.set(name, value);
  } else {
    throw new Error(`Invalid Stage 6 branch-protection argument ${name}.`);
  }
  index += 2;
}

const required = (name: string): string => {
  const value = values.get(name);
  if (!value) throw new Error(`Missing Stage 6 branch-protection argument ${name}.`);
  return value;
};
const plan = createStage6SourceBranchProtectionPlan({
  repository: required("--repository"),
  branch: required("--branch"),
  expectedHeadCommit: required("--expected-head"),
  requiredStatusChecks: checks,
});
const planSha256 = hashStage6SourceBranchProtectionPlan(plan);

if (!apply) {
  if (values.has("--confirm-sha256")) {
    throw new Error("--confirm-sha256 is only valid together with --apply.");
  }
  process.stdout.write(`${JSON.stringify({ plan, planSha256 }, null, 2)}\n`);
} else {
  const receipt = applyStage6SourceBranchProtection({
    plan,
    confirmationSha256: required("--confirm-sha256"),
  });
  process.stdout.write(`${JSON.stringify(receipt, null, 2)}\n`);
}
