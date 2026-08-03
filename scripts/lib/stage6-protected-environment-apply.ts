import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  closeSync,
  constants,
  fstatSync,
  lstatSync,
  mkdtempSync,
  openSync,
  readFileSync,
  realpathSync,
  rmSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join, resolve } from "node:path";
import { isDeepStrictEqual, TextDecoder } from "node:util";

import {
  type Stage6ProtectedEnvironmentConfig,
  STAGE6_PROTECTED_ENVIRONMENT_CONFIG_ASSESSMENT,
  STAGE6_PROTECTED_ENVIRONMENT_CONFIG_SCHEMA,
  createStage6ProtectedEnvironmentConfig,
} from "./stage6-protected-environment-config.ts";

const REPOSITORY_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const BRANCH_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,198}[A-Za-z0-9._-])?$/;
const MAX_CONFIGURATION_BYTES = 128 * 1024;
const MAX_GH_OUTPUT_BYTES = 2 * 1024 * 1024;
const ENVIRONMENT = "stage6-enterprise-ga" as const;

export const STAGE6_PROTECTED_ENVIRONMENT_APPLICATION_SCHEMA =
  "synara.stage6-protected-environment-application.v1";
export const STAGE6_PROTECTED_ENVIRONMENT_APPLICATION_ASSESSMENT =
  "configuration-applied-not-release-approved";

const VARIABLE_NAMES = [
  "SYNARA_STAGE6_CANDIDATE_ID",
  "SYNARA_STAGE6_CANDIDATE_SOURCE_COMMIT",
  "SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID",
  "SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_SHA256",
  "SYNARA_STAGE6_DESKTOP_ARTIFACT_SET_SHA256",
] as const;
const SECRET_NAME = "SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64" as const;

export interface Stage6EnvironmentReviewer {
  readonly type: "User" | "Team";
  readonly id: number;
}

export interface Stage6GhCommandInput {
  readonly args: ReadonlyArray<string>;
  readonly stdin?: string;
  readonly sensitive?: boolean;
}

export interface Stage6GhCommandResult {
  readonly stdout: string;
  readonly stderr: string;
}

export interface Stage6ProtectedEnvironmentApplyDependencies {
  readonly runGh?: (input: Stage6GhCommandInput) => Stage6GhCommandResult;
  readonly readConfiguration?: (path: string) => {
    readonly config: Stage6ProtectedEnvironmentConfig;
    readonly sha256: string;
  };
}

export interface Stage6ProtectedEnvironmentApplicationReceipt {
  readonly schemaVersion: typeof STAGE6_PROTECTED_ENVIRONMENT_APPLICATION_SCHEMA;
  readonly repository: string;
  readonly environment: typeof ENVIRONMENT;
  readonly configurationSha256: string;
  readonly candidateId: string;
  readonly buildRunId: string;
  readonly sourceBranch: string;
  readonly reviewers: ReadonlyArray<Stage6EnvironmentReviewer>;
  readonly protection: {
    readonly preventSelfReview: true;
    readonly administratorBypassDisabled: true;
    readonly protectedBranchesOnly: true;
  };
  readonly appliedVariableNames: ReadonlyArray<(typeof VARIABLE_NAMES)[number]>;
  readonly appliedSecretNames: readonly [typeof SECRET_NAME];
  readonly verification: {
    readonly exactConfigurationRevalidated: true;
    readonly exactWorkflowRunReadBeforeWrite: true;
    readonly sourceBranchProtectionReadBeforeWrite: true;
    readonly environmentReadBeforeWrite: true;
    readonly environmentReadAfterWrite: true;
    readonly variablesReadAfterWrite: true;
    readonly secretPresenceReadAfterWrite: true;
    readonly secretValueReadableFromGitHub: false;
  };
  readonly assessment: typeof STAGE6_PROTECTED_ENVIRONMENT_APPLICATION_ASSESSMENT;
}

function requireObject(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object.`);
  }
  return value as Record<string, unknown>;
}

function requireString(value: unknown, label: string): string {
  if (typeof value !== "string" || value.length === 0) {
    throw new Error(`${label} must be a non-empty string.`);
  }
  return value;
}

function parseJson(value: string, label: string): unknown {
  try {
    return JSON.parse(value);
  } catch (error) {
    throw new Error(`${label} must be valid JSON.`, { cause: error });
  }
}

function readPreparedConfiguration(pathInput: string): {
  readonly config: Stage6ProtectedEnvironmentConfig;
  readonly sha256: string;
} {
  const path = resolve(pathInput);
  const entry = lstatSync(path);
  if (!entry.isFile() || entry.isSymbolicLink() || entry.size <= 0) {
    throw new Error("Protected environment configuration must be a non-empty regular file.");
  }
  if (entry.size > MAX_CONFIGURATION_BYTES) {
    throw new Error("Protected environment configuration exceeds its bounded size.");
  }
  const descriptor = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  let bytes: Buffer;
  try {
    const opened = fstatSync(descriptor);
    if (
      !opened.isFile() ||
      opened.dev !== entry.dev ||
      opened.ino !== entry.ino ||
      opened.size !== entry.size
    ) {
      throw new Error("Protected environment configuration changed while it was opened.");
    }
    bytes = readFileSync(descriptor);
    const after = fstatSync(descriptor);
    if (
      bytes.byteLength !== opened.size ||
      after.size !== opened.size ||
      after.mtimeMs !== opened.mtimeMs ||
      after.ctimeMs !== opened.ctimeMs
    ) {
      throw new Error("Protected environment configuration changed while it was read.");
    }
  } finally {
    closeSync(descriptor);
  }
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch (error) {
    throw new Error("Protected environment configuration must be valid UTF-8.", { cause: error });
  }
  const parsed = requireObject(
    parseJson(text, "Protected environment configuration"),
    "Protected environment configuration",
  );
  if (
    parsed.schemaVersion !== STAGE6_PROTECTED_ENVIRONMENT_CONFIG_SCHEMA ||
    parsed.assessment !== STAGE6_PROTECTED_ENVIRONMENT_CONFIG_ASSESSMENT ||
    parsed.environment !== ENVIRONMENT
  ) {
    throw new Error("Protected environment configuration identity is invalid.");
  }
  const variables = requireObject(parsed.variables, "Protected environment variables");
  const secrets = requireObject(parsed.secrets, "Protected environment secrets");
  const receipt = requireObject(parsed.receipt, "Protected environment receipt projection");
  const receiptFileName = requireString(
    receipt.fileName,
    "Protected environment receipt file name",
  );
  if (
    !/^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$/.test(receiptFileName) ||
    basename(receiptFileName) !== receiptFileName
  ) {
    throw new Error("Protected environment receipt file name must be a safe basename.");
  }
  const receiptBase64 = requireString(secrets[SECRET_NAME], SECRET_NAME);
  if (!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(receiptBase64)) {
    throw new Error("Protected environment candidate receipt must use canonical base64.");
  }
  const receiptBytes = Buffer.from(receiptBase64, "base64");
  if (receiptBytes.toString("base64") !== receiptBase64) {
    throw new Error("Protected environment candidate receipt base64 is not canonical.");
  }

  const temporaryRoot = mkdtempSync(join(tmpdir(), "synara-stage6-environment-apply-"));
  const receiptPath = join(temporaryRoot, receiptFileName);
  try {
    writeFileSync(receiptPath, receiptBytes, { flag: "wx", mode: 0o600 });
    const expected = createStage6ProtectedEnvironmentConfig({
      candidateReceiptPath: receiptPath,
      expectedCandidateId: requireString(
        variables.SYNARA_STAGE6_CANDIDATE_ID,
        "SYNARA_STAGE6_CANDIDATE_ID",
      ),
      expectedSourceCommit: requireString(
        variables.SYNARA_STAGE6_CANDIDATE_SOURCE_COMMIT,
        "SYNARA_STAGE6_CANDIDATE_SOURCE_COMMIT",
      ),
      buildRunId: requireString(
        variables.SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID,
        "SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID",
      ),
    });
    if (!isDeepStrictEqual(parsed, expected)) {
      throw new Error(
        "Protected environment configuration differs from the canonical verified projection.",
      );
    }
    return {
      config: expected,
      sha256: `sha256:${createHash("sha256").update(bytes).digest("hex")}`,
    };
  } finally {
    rmSync(temporaryRoot, { recursive: true, force: true });
  }
}

function validateReviewers(
  reviewersInput: ReadonlyArray<Stage6EnvironmentReviewer>,
): ReadonlyArray<Stage6EnvironmentReviewer> {
  if (reviewersInput.length < 2 || reviewersInput.length > 6) {
    throw new Error("Stage 6 environment requires between two and six reviewers.");
  }
  const keys = new Set<string>();
  const reviewers = reviewersInput.map((reviewer) => {
    if (
      (reviewer.type !== "User" && reviewer.type !== "Team") ||
      !Number.isSafeInteger(reviewer.id) ||
      reviewer.id <= 0
    ) {
      throw new Error("Stage 6 environment reviewer is invalid.");
    }
    const key = `${reviewer.type}:${reviewer.id}`;
    if (keys.has(key)) throw new Error("Stage 6 environment reviewers must be unique.");
    keys.add(key);
    return Object.freeze({ type: reviewer.type, id: reviewer.id });
  });
  return Object.freeze(reviewers);
}

function defaultRunGh(input: Stage6GhCommandInput): Stage6GhCommandResult {
  const result = spawnSync("gh", [...input.args], {
    encoding: "utf8",
    input: input.stdin,
    maxBuffer: MAX_GH_OUTPUT_BYTES,
    shell: false,
    env: { ...process.env, GH_PROMPT_DISABLED: "1" },
  });
  if (result.error) throw new Error(`GitHub CLI could not start: ${result.error.message}`);
  if (result.status !== 0) {
    if (input.sensitive) {
      throw new Error(`GitHub CLI secret update failed with exit status ${result.status}.`);
    }
    throw new Error(
      `GitHub CLI command failed with exit status ${result.status}: ${(result.stderr || result.stdout).trim()}`,
    );
  }
  return { stdout: result.stdout, stderr: result.stderr };
}

function environmentEndpoint(repository: string): string {
  return `repos/${repository}/environments/${ENVIRONMENT}`;
}

export function verifyStage6WorkflowRunResponse(
  raw: unknown,
  expected: {
    readonly repository: string;
    readonly runId: string;
    readonly sourceCommit: string;
  },
): string {
  const run = requireObject(raw, "GitHub workflow run response");
  if (String(run.id) !== expected.runId || run.run_attempt !== 1) {
    throw new Error("GitHub workflow run does not identify the exact fresh candidate run.");
  }
  if (
    run.event !== "workflow_dispatch" ||
    run.path !== ".github/workflows/release.yml" ||
    run.head_sha !== expected.sourceCommit
  ) {
    throw new Error(
      "GitHub workflow run does not bind the exact release workflow and source commit.",
    );
  }
  const runRepository = requireObject(run.repository, "GitHub workflow run repository");
  if (
    typeof runRepository.full_name !== "string" ||
    runRepository.full_name.toLowerCase() !== expected.repository.toLowerCase()
  ) {
    throw new Error("GitHub workflow run belongs to a different repository.");
  }
  if (
    run.status !== "requested" &&
    run.status !== "queued" &&
    run.status !== "pending" &&
    run.status !== "waiting" &&
    run.status !== "in_progress"
  ) {
    throw new Error("GitHub workflow run is no longer an active candidate run.");
  }
  const sourceBranch = requireString(run.head_branch, "GitHub workflow run source branch");
  if (!BRANCH_PATTERN.test(sourceBranch) || sourceBranch.includes("..")) {
    throw new Error("GitHub workflow run source branch is invalid.");
  }
  return sourceBranch;
}

export function verifyStage6SourceBranchProtectionResponse(raw: unknown): void {
  const protection = requireObject(raw, "GitHub source branch protection response");
  const enforceAdmins = requireObject(protection.enforce_admins, "GitHub enforce-admins rule");
  const pullRequests = requireObject(
    protection.required_pull_request_reviews,
    "GitHub required pull-request reviews",
  );
  const statusChecks = requireObject(
    protection.required_status_checks,
    "GitHub required status checks",
  );
  const forcePushes = requireObject(protection.allow_force_pushes, "GitHub force-push policy");
  const deletions = requireObject(protection.allow_deletions, "GitHub branch-deletion policy");
  if (
    enforceAdmins.enabled !== true ||
    pullRequests.dismiss_stale_reviews !== true ||
    !Number.isSafeInteger(pullRequests.required_approving_review_count) ||
    (pullRequests.required_approving_review_count as number) < 1 ||
    statusChecks.strict !== true ||
    forcePushes.enabled !== false ||
    deletions.enabled !== false
  ) {
    throw new Error(
      "Stage 6 source branch must enforce administrators, stale-review dismissal, approval, strict status checks, and deny force-push/deletion.",
    );
  }
  const checks = Array.isArray(statusChecks.checks)
    ? statusChecks.checks
    : Array.isArray(statusChecks.contexts)
      ? statusChecks.contexts
      : [];
  if (checks.length === 0) {
    throw new Error("Stage 6 source branch must require at least one status check.");
  }
}

function requireAdministratorBypassDisabled(raw: unknown): Record<string, unknown> {
  const environment = requireObject(raw, "GitHub environment response");
  if (environment.name !== ENVIRONMENT || environment.can_admins_bypass !== false) {
    throw new Error(
      "Stage 6 GitHub Environment must already exist with administrator bypass disabled in GitHub settings.",
    );
  }
  return environment;
}

function verifyAppliedEnvironment(
  raw: unknown,
  expectedReviewers: ReadonlyArray<Stage6EnvironmentReviewer>,
): void {
  const environment = requireAdministratorBypassDisabled(raw);
  const branchPolicy = requireObject(
    environment.deployment_branch_policy,
    "GitHub environment deployment branch policy",
  );
  if (branchPolicy.protected_branches !== true || branchPolicy.custom_branch_policies !== false) {
    throw new Error("Stage 6 GitHub Environment must allow protected branches only.");
  }
  if (!Array.isArray(environment.protection_rules)) {
    throw new Error("GitHub environment protection rules must be an array.");
  }
  const reviewerRules = environment.protection_rules.filter(
    (rule) =>
      requireObject(rule, "GitHub environment protection rule").type === "required_reviewers",
  );
  if (reviewerRules.length !== 1) {
    throw new Error("Stage 6 GitHub Environment must have exactly one reviewer rule.");
  }
  const reviewerRule = requireObject(reviewerRules[0], "GitHub environment reviewer rule");
  if (reviewerRule.prevent_self_review !== true || !Array.isArray(reviewerRule.reviewers)) {
    throw new Error("Stage 6 GitHub Environment must prevent self review.");
  }
  const actualKeys = reviewerRule.reviewers.map((rawReviewer) => {
    const wrapper = requireObject(rawReviewer, "GitHub environment reviewer");
    const reviewer = requireObject(wrapper.reviewer, "GitHub environment reviewer identity");
    return `${String(wrapper.type)}:${String(reviewer.id)}`;
  });
  const expectedKeys = expectedReviewers.map((reviewer) => `${reviewer.type}:${reviewer.id}`);
  if (
    actualKeys.length !== expectedKeys.length ||
    actualKeys.toSorted().some((key, index) => key !== expectedKeys.toSorted()[index])
  ) {
    throw new Error("Stage 6 GitHub Environment reviewers do not match the requested identities.");
  }
}

function verifyVariables(raw: unknown, config: Stage6ProtectedEnvironmentConfig): void {
  if (!Array.isArray(raw)) throw new Error("GitHub environment variable list must be an array.");
  const values = new Map(
    raw.map((entry) => {
      const variable = requireObject(entry, "GitHub environment variable");
      return [
        requireString(variable.name, "GitHub environment variable name"),
        requireString(variable.value, "GitHub environment variable value"),
      ] as const;
    }),
  );
  for (const name of VARIABLE_NAMES) {
    if (values.get(name) !== config.variables[name]) {
      throw new Error(`GitHub environment variable ${name} does not match the candidate.`);
    }
  }
}

function verifySecretPresence(raw: unknown): void {
  if (!Array.isArray(raw)) throw new Error("GitHub environment secret list must be an array.");
  const names = raw.map((entry) =>
    requireString(requireObject(entry, "GitHub environment secret").name, "GitHub secret name"),
  );
  if (names.filter((name) => name === SECRET_NAME).length !== 1) {
    throw new Error(`GitHub environment secret ${SECRET_NAME} is not present exactly once.`);
  }
}

export function applyStage6ProtectedEnvironmentConfiguration(
  input: {
    readonly configurationPath: string;
    readonly repository: string;
    readonly reviewers: ReadonlyArray<Stage6EnvironmentReviewer>;
  },
  dependencies: Stage6ProtectedEnvironmentApplyDependencies = {},
): Stage6ProtectedEnvironmentApplicationReceipt {
  if (!REPOSITORY_PATTERN.test(input.repository)) throw new Error("GitHub repository is invalid.");
  const reviewers = validateReviewers(input.reviewers);
  const prepared = (dependencies.readConfiguration ?? readPreparedConfiguration)(
    input.configurationPath,
  );
  const runGh = dependencies.runGh ?? defaultRunGh;
  const endpoint = environmentEndpoint(input.repository);

  const workflowRun = runGh({
    args: [
      "api",
      `repos/${input.repository}/actions/runs/${prepared.config.variables.SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID}`,
    ],
  });
  const sourceBranch = verifyStage6WorkflowRunResponse(
    parseJson(workflowRun.stdout, "GitHub workflow run response"),
    {
      repository: input.repository,
      runId: prepared.config.variables.SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID,
      sourceCommit: prepared.config.variables.SYNARA_STAGE6_CANDIDATE_SOURCE_COMMIT,
    },
  );
  const branchProtection = runGh({
    args: [
      "api",
      `repos/${input.repository}/branches/${encodeURIComponent(sourceBranch)}/protection`,
    ],
  });
  verifyStage6SourceBranchProtectionResponse(
    parseJson(branchProtection.stdout, "GitHub source branch protection response"),
  );

  const before = runGh({ args: ["api", endpoint] });
  requireAdministratorBypassDisabled(parseJson(before.stdout, "GitHub environment response"));

  const environmentRequest = JSON.stringify({
    wait_timer: 0,
    prevent_self_review: true,
    reviewers,
    deployment_branch_policy: {
      protected_branches: true,
      custom_branch_policies: false,
    },
  });
  const applied = runGh({
    args: ["api", "--method", "PUT", endpoint, "--input", "-"],
    stdin: environmentRequest,
  });
  verifyAppliedEnvironment(
    parseJson(applied.stdout, "Updated GitHub environment response"),
    reviewers,
  );

  for (const name of VARIABLE_NAMES) {
    runGh({
      args: ["variable", "set", name, "--env", ENVIRONMENT, "--repo", input.repository],
      stdin: prepared.config.variables[name],
    });
  }
  runGh({
    args: ["secret", "set", SECRET_NAME, "--env", ENVIRONMENT, "--repo", input.repository],
    stdin: prepared.config.secrets[SECRET_NAME],
    sensitive: true,
  });

  const variables = runGh({
    args: [
      "variable",
      "list",
      "--env",
      ENVIRONMENT,
      "--repo",
      input.repository,
      "--json",
      "name,value",
    ],
  });
  verifyVariables(parseJson(variables.stdout, "GitHub environment variable list"), prepared.config);
  const secrets = runGh({
    args: ["secret", "list", "--env", ENVIRONMENT, "--repo", input.repository, "--json", "name"],
  });
  verifySecretPresence(parseJson(secrets.stdout, "GitHub environment secret list"));
  const after = runGh({ args: ["api", endpoint] });
  verifyAppliedEnvironment(parseJson(after.stdout, "Final GitHub environment response"), reviewers);

  return {
    schemaVersion: STAGE6_PROTECTED_ENVIRONMENT_APPLICATION_SCHEMA,
    repository: input.repository,
    environment: ENVIRONMENT,
    configurationSha256: prepared.sha256,
    candidateId: prepared.config.variables.SYNARA_STAGE6_CANDIDATE_ID,
    buildRunId: prepared.config.variables.SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID,
    sourceBranch,
    reviewers,
    protection: {
      preventSelfReview: true,
      administratorBypassDisabled: true,
      protectedBranchesOnly: true,
    },
    appliedVariableNames: VARIABLE_NAMES,
    appliedSecretNames: [SECRET_NAME],
    verification: {
      exactConfigurationRevalidated: true,
      exactWorkflowRunReadBeforeWrite: true,
      sourceBranchProtectionReadBeforeWrite: true,
      environmentReadBeforeWrite: true,
      environmentReadAfterWrite: true,
      variablesReadAfterWrite: true,
      secretPresenceReadAfterWrite: true,
      secretValueReadableFromGitHub: false,
    },
    assessment: STAGE6_PROTECTED_ENVIRONMENT_APPLICATION_ASSESSMENT,
  };
}

export function writeStage6ProtectedEnvironmentApplicationReceipt(
  outputPath: string,
  receipt: Stage6ProtectedEnvironmentApplicationReceipt,
): { readonly path: string; readonly sha256Path: string; readonly sha256: string } {
  const requestedPath = resolve(outputPath);
  const path = join(realpathSync(dirname(requestedPath)), basename(requestedPath));
  const sha256Path = `${path}.sha256`;
  const bytes = Buffer.from(`${JSON.stringify(receipt, null, 2)}\n`, "utf8");
  const sha256 = createHash("sha256").update(bytes).digest("hex");
  writeFileSync(path, bytes, { flag: "wx", mode: 0o600 });
  try {
    writeFileSync(sha256Path, `${sha256}  ${basename(path)}\n`, {
      flag: "wx",
      mode: 0o600,
    });
  } catch (error) {
    try {
      unlinkSync(path);
    } catch (rollbackError) {
      // eslint-disable-next-line preserve-caught-error -- AggregateError retains both failures and names the primary as cause.
      throw new AggregateError(
        [error, rollbackError],
        "Stage 6 environment application receipt rollback failed.",
        { cause: error },
      );
    }
    throw error;
  }
  return { path, sha256Path, sha256 };
}
