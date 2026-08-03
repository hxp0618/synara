import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";

import {
  type Stage6EnvironmentReviewer,
  verifyStage6SourceBranchProtectionResponse,
} from "./stage6-protected-environment-apply.ts";

const REPOSITORY_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const BRANCH_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,198}[A-Za-z0-9._-])?$/;
const COMMIT_PATTERN = /^[0-9a-f]{40}$/;
const MAX_GH_OUTPUT_BYTES = 2 * 1024 * 1024;
const ENVIRONMENT = "stage6-enterprise-ga" as const;

export const STAGE6_ENVIRONMENT_BASELINE_PLAN_SCHEMA = "synara.stage6-environment-baseline-plan.v1";
export const STAGE6_ENVIRONMENT_BASELINE_APPLICATION_SCHEMA =
  "synara.stage6-environment-baseline-application.v1";

export interface Stage6EnvironmentBaselineCommandResult {
  readonly status: number;
  readonly stdout: string;
  readonly stderr: string;
}

export interface Stage6EnvironmentBaselinePlan {
  readonly schemaVersion: typeof STAGE6_ENVIRONMENT_BASELINE_PLAN_SCHEMA;
  readonly repository: string;
  readonly sourceBranch: string;
  readonly expectedHeadCommit: string;
  readonly environment: typeof ENVIRONMENT;
  readonly reviewers: ReadonlyArray<Stage6EnvironmentReviewer>;
  readonly protection: {
    readonly waitTimerMinutes: 0;
    readonly preventSelfReview: true;
    readonly protectedBranchesOnly: true;
  };
  readonly assessment: "environment-baseline-planned-not-applied";
}

export interface Stage6EnvironmentBaselineApplicationReceipt {
  readonly schemaVersion: typeof STAGE6_ENVIRONMENT_BASELINE_APPLICATION_SCHEMA;
  readonly repository: string;
  readonly sourceBranch: string;
  readonly expectedHeadCommit: string;
  readonly environment: typeof ENVIRONMENT;
  readonly planSha256: string;
  readonly reviewers: ReadonlyArray<Stage6EnvironmentReviewer>;
  readonly environmentExistedBeforeApply: boolean;
  readonly changed: boolean;
  readonly administratorBypassDisabled: boolean;
  readonly manualAdministratorBypassActionRequired: boolean;
  readonly verification: {
    readonly repositoryAdminAuthorityReadBeforeWrite: true;
    readonly exactBranchHeadReadBeforeWrite: true;
    readonly sourceBranchProtectionReadBeforeWrite: true;
    readonly environmentReadBeforeWrite: true;
    readonly environmentReadAfterWrite: true;
    readonly exactReviewersReadAfterWrite: true;
    readonly protectedBranchesOnlyReadAfterWrite: true;
    readonly branchHeadStableAfterWrite: true;
    readonly administratorBypassWriteAvailableInGitHubApi: false;
  };
  readonly assessment:
    | "environment-baseline-ready-not-release-approved"
    | "environment-baseline-applied-manual-admin-bypass-required";
}

export interface Stage6EnvironmentBaselineDependencies {
  readonly runGh?: (
    args: ReadonlyArray<string>,
    stdin?: string,
  ) => Stage6EnvironmentBaselineCommandResult;
}

function requireObject(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object.`);
  }
  return value as Record<string, unknown>;
}

function parseJson(value: string, label: string): unknown {
  try {
    return JSON.parse(value);
  } catch (error) {
    throw new Error(`${label} returned invalid JSON.`, { cause: error });
  }
}

function defaultRunGh(
  args: ReadonlyArray<string>,
  stdin?: string,
): Stage6EnvironmentBaselineCommandResult {
  const result = spawnSync("gh", [...args], {
    encoding: "utf8",
    input: stdin,
    maxBuffer: MAX_GH_OUTPUT_BYTES,
    shell: false,
    env: { ...process.env, GH_PROMPT_DISABLED: "1" },
  });
  if (result.error) throw new Error(`GitHub CLI could not start: ${result.error.message}`);
  return {
    status: result.status ?? 1,
    stdout: result.stdout,
    stderr: result.stderr,
  };
}

function requireSuccess(result: Stage6EnvironmentBaselineCommandResult, label: string): string {
  if (result.status !== 0) {
    throw new Error(
      `${label} failed with exit status ${result.status}: ${(result.stderr || result.stdout).trim()}`,
    );
  }
  return result.stdout;
}

function isNotFound(result: Stage6EnvironmentBaselineCommandResult): boolean {
  return result.status !== 0 && /(?:HTTP\s+404|Not Found)/i.test(result.stderr || result.stdout);
}

function normalizeReviewers(
  reviewers: ReadonlyArray<Stage6EnvironmentReviewer>,
): ReadonlyArray<Stage6EnvironmentReviewer> {
  if (reviewers.length < 2 || reviewers.length > 6) {
    throw new Error("Stage 6 Environment requires between two and six reviewers.");
  }
  const keys = new Set<string>();
  const normalized = reviewers.map((reviewer) => {
    if (
      (reviewer.type !== "User" && reviewer.type !== "Team") ||
      !Number.isSafeInteger(reviewer.id) ||
      reviewer.id <= 0
    ) {
      throw new Error("Stage 6 Environment reviewer is invalid.");
    }
    const key = `${reviewer.type}:${reviewer.id}`;
    if (keys.has(key)) throw new Error("Stage 6 Environment reviewers must be unique.");
    keys.add(key);
    return Object.freeze({ type: reviewer.type, id: reviewer.id });
  });
  return Object.freeze(
    normalized.toSorted((left, right) => {
      const typeOrder = left.type.localeCompare(right.type);
      return typeOrder === 0 ? left.id - right.id : typeOrder;
    }),
  );
}

function validateIdentity(input: {
  readonly repository: string;
  readonly sourceBranch: string;
  readonly expectedHeadCommit: string;
}): void {
  if (!REPOSITORY_PATTERN.test(input.repository)) throw new Error("GitHub repository is invalid.");
  if (
    !BRANCH_PATTERN.test(input.sourceBranch) ||
    input.sourceBranch.includes("..") ||
    input.sourceBranch.includes("//")
  ) {
    throw new Error("GitHub source branch is invalid.");
  }
  if (!COMMIT_PATTERN.test(input.expectedHeadCommit)) {
    throw new Error("Expected GitHub source branch HEAD commit is invalid.");
  }
}

export function createStage6EnvironmentBaselinePlan(input: {
  readonly repository: string;
  readonly sourceBranch: string;
  readonly expectedHeadCommit: string;
  readonly reviewers: ReadonlyArray<Stage6EnvironmentReviewer>;
}): Stage6EnvironmentBaselinePlan {
  validateIdentity(input);
  return Object.freeze({
    schemaVersion: STAGE6_ENVIRONMENT_BASELINE_PLAN_SCHEMA,
    repository: input.repository,
    sourceBranch: input.sourceBranch,
    expectedHeadCommit: input.expectedHeadCommit,
    environment: ENVIRONMENT,
    reviewers: normalizeReviewers(input.reviewers),
    protection: Object.freeze({
      waitTimerMinutes: 0,
      preventSelfReview: true,
      protectedBranchesOnly: true,
    }),
    assessment: "environment-baseline-planned-not-applied",
  });
}

export function hashStage6EnvironmentBaselinePlan(plan: Stage6EnvironmentBaselinePlan): string {
  return `sha256:${createHash("sha256").update(JSON.stringify(plan)).digest("hex")}`;
}

function branchHead(raw: unknown): string {
  const branch = requireObject(raw, "GitHub branch response");
  const commit = requireObject(branch.commit, "GitHub branch commit");
  if (typeof commit.sha !== "string" || !COMMIT_PATTERN.test(commit.sha)) {
    throw new Error("GitHub branch response has an invalid HEAD commit.");
  }
  return commit.sha;
}

function requireRepositoryAdmin(raw: unknown, repository: string): void {
  const response = requireObject(raw, "GitHub repository response");
  if (
    typeof response.full_name !== "string" ||
    response.full_name.toLowerCase() !== repository.toLowerCase() ||
    response.archived === true ||
    response.disabled === true
  ) {
    throw new Error("GitHub repository identity or lifecycle is invalid.");
  }
  const permissions = requireObject(response.permissions, "GitHub repository permissions");
  if (permissions.admin !== true) {
    throw new Error("GitHub repository administrator authority is required.");
  }
}

interface EnvironmentAssessment {
  readonly administratorBypassDisabled: boolean;
  readonly waitTimerMinutes: number;
  readonly preventSelfReview: boolean;
  readonly protectedBranchesOnly: boolean;
  readonly reviewers: ReadonlyArray<Stage6EnvironmentReviewer>;
}

function assessEnvironment(raw: unknown): EnvironmentAssessment {
  const environment = requireObject(raw, "GitHub Environment response");
  if (environment.name !== ENVIRONMENT || !Array.isArray(environment.protection_rules)) {
    throw new Error("GitHub Environment response identity is invalid.");
  }
  const branchPolicy =
    environment.deployment_branch_policy === null ||
    environment.deployment_branch_policy === undefined
      ? {}
      : requireObject(
          environment.deployment_branch_policy,
          "GitHub Environment deployment branch policy",
        );
  const reviewerRules = environment.protection_rules.filter(
    (rule) =>
      requireObject(rule, "GitHub Environment protection rule").type === "required_reviewers",
  );
  if (reviewerRules.length > 1) {
    throw new Error("GitHub Environment has multiple reviewer rules.");
  }
  const reviewerRule = reviewerRules[0]
    ? requireObject(reviewerRules[0], "GitHub Environment reviewer rule")
    : undefined;
  const reviewers = Array.isArray(reviewerRule?.reviewers)
    ? reviewerRule.reviewers.map((entry) => {
        const wrapper = requireObject(entry, "GitHub Environment reviewer");
        const identity = requireObject(wrapper.reviewer, "GitHub Environment reviewer identity");
        if (
          (wrapper.type !== "User" && wrapper.type !== "Team") ||
          !Number.isSafeInteger(identity.id) ||
          (identity.id as number) <= 0
        ) {
          throw new Error("GitHub Environment reviewer identity is invalid.");
        }
        return { type: wrapper.type as "User" | "Team", id: identity.id as number };
      })
    : [];
  const observedReviewerKeys = new Set<string>();
  for (const reviewer of reviewers) {
    const key = `${reviewer.type}:${reviewer.id}`;
    if (observedReviewerKeys.has(key)) {
      throw new Error("GitHub Environment reviewers must be unique.");
    }
    observedReviewerKeys.add(key);
  }
  const waitRules = environment.protection_rules.filter(
    (rule) => requireObject(rule, "GitHub Environment protection rule").type === "wait_timer",
  );
  if (waitRules.length > 1) throw new Error("GitHub Environment has multiple wait-timer rules.");
  const waitTimer = waitRules[0]
    ? requireObject(waitRules[0], "GitHub Environment wait-timer rule").wait_timer
    : 0;
  if (!Number.isSafeInteger(waitTimer) || (waitTimer as number) < 0) {
    throw new Error("GitHub Environment wait timer is invalid.");
  }
  return {
    administratorBypassDisabled: environment.can_admins_bypass === false,
    waitTimerMinutes: waitTimer as number,
    preventSelfReview: reviewerRule?.prevent_self_review === true,
    protectedBranchesOnly:
      branchPolicy.protected_branches === true && branchPolicy.custom_branch_policies === false,
    // Observed reviewers keep only identity validation: a drifted environment (for example one
    // GitHub silently reduced below the planned count) must surface as a reviewer-set mismatch,
    // not fail plan-input count validation before the safety comparison can run.
    reviewers,
  };
}

function reviewerKeys(reviewers: ReadonlyArray<Stage6EnvironmentReviewer>): ReadonlyArray<string> {
  return reviewers.map((reviewer) => `${reviewer.type}:${reviewer.id}`).toSorted();
}

function exactBaseline(
  assessment: EnvironmentAssessment,
  reviewers: ReadonlyArray<Stage6EnvironmentReviewer>,
): boolean {
  return (
    assessment.waitTimerMinutes === 0 &&
    assessment.preventSelfReview &&
    assessment.protectedBranchesOnly &&
    JSON.stringify(reviewerKeys(assessment.reviewers)) === JSON.stringify(reviewerKeys(reviewers))
  );
}

function assertExistingEnvironmentCanBeSafelyUpdated(
  assessment: EnvironmentAssessment,
  reviewers: ReadonlyArray<Stage6EnvironmentReviewer>,
): void {
  if (
    assessment.waitTimerMinutes > 0 ||
    (assessment.reviewers.length > 0 &&
      JSON.stringify(reviewerKeys(assessment.reviewers)) !==
        JSON.stringify(reviewerKeys(reviewers)))
  ) {
    throw new Error(
      "Existing GitHub Environment contains a wait timer or different reviewers that this plan would remove.",
    );
  }
}

function environmentPayload(reviewers: ReadonlyArray<Stage6EnvironmentReviewer>): string {
  return JSON.stringify({
    wait_timer: 0,
    prevent_self_review: true,
    reviewers,
    deployment_branch_policy: {
      protected_branches: true,
      custom_branch_policies: false,
    },
  });
}

export function applyStage6EnvironmentBaseline(
  input: {
    readonly plan: Stage6EnvironmentBaselinePlan;
    readonly confirmationSha256: string;
  },
  dependencies: Stage6EnvironmentBaselineDependencies = {},
): Stage6EnvironmentBaselineApplicationReceipt {
  const canonicalPlan = createStage6EnvironmentBaselinePlan(input.plan);
  if (
    input.plan.schemaVersion !== STAGE6_ENVIRONMENT_BASELINE_PLAN_SCHEMA ||
    input.plan.environment !== ENVIRONMENT ||
    input.plan.assessment !== "environment-baseline-planned-not-applied" ||
    JSON.stringify(input.plan) !== JSON.stringify(canonicalPlan)
  ) {
    throw new Error("Stage 6 Environment baseline plan is not canonical.");
  }
  const planSha256 = hashStage6EnvironmentBaselinePlan(canonicalPlan);
  if (input.confirmationSha256 !== planSha256) {
    throw new Error("Stage 6 Environment baseline confirmation digest does not match.");
  }
  const runGh = dependencies.runGh ?? defaultRunGh;
  requireRepositoryAdmin(
    parseJson(
      requireSuccess(
        runGh(["api", `repos/${canonicalPlan.repository}`]),
        "GitHub repository inspection",
      ),
      "GitHub repository inspection",
    ),
    canonicalPlan.repository,
  );
  const branchEndpoint = `repos/${canonicalPlan.repository}/branches/${encodeURIComponent(canonicalPlan.sourceBranch)}`;
  if (
    branchHead(
      parseJson(
        requireSuccess(runGh(["api", branchEndpoint]), "GitHub source branch inspection"),
        "GitHub source branch inspection",
      ),
    ) !== canonicalPlan.expectedHeadCommit
  ) {
    throw new Error("GitHub source branch HEAD differs from the confirmed Environment plan.");
  }
  verifyStage6SourceBranchProtectionResponse(
    parseJson(
      requireSuccess(
        runGh(["api", `${branchEndpoint}/protection`]),
        "GitHub source branch protection inspection",
      ),
      "GitHub source branch protection inspection",
    ),
  );

  const environmentEndpoint = `repos/${canonicalPlan.repository}/environments/${ENVIRONMENT}`;
  const before = runGh(["api", environmentEndpoint]);
  let environmentExistedBeforeApply = false;
  let beforeAssessment: EnvironmentAssessment | undefined;
  if (before.status === 0) {
    environmentExistedBeforeApply = true;
    beforeAssessment = assessEnvironment(parseJson(before.stdout, "GitHub Environment inspection"));
  } else if (!isNotFound(before)) {
    requireSuccess(before, "GitHub Environment inspection");
  }

  let changed = false;
  if (!beforeAssessment || !exactBaseline(beforeAssessment, canonicalPlan.reviewers)) {
    if (beforeAssessment) {
      assertExistingEnvironmentCanBeSafelyUpdated(beforeAssessment, canonicalPlan.reviewers);
    }
    const applied = assessEnvironment(
      parseJson(
        requireSuccess(
          runGh(
            ["api", "--method", "PUT", environmentEndpoint, "--input", "-"],
            environmentPayload(canonicalPlan.reviewers),
          ),
          "GitHub Environment baseline update",
        ),
        "GitHub Environment baseline update",
      ),
    );
    if (!exactBaseline(applied, canonicalPlan.reviewers)) {
      throw new Error("GitHub Environment baseline update did not apply the exact planned policy.");
    }
    changed = true;
  }

  const after = assessEnvironment(
    parseJson(
      requireSuccess(runGh(["api", environmentEndpoint]), "Final GitHub Environment inspection"),
      "Final GitHub Environment inspection",
    ),
  );
  if (!exactBaseline(after, canonicalPlan.reviewers)) {
    throw new Error("Final GitHub Environment policy differs from the confirmed baseline plan.");
  }
  if (
    branchHead(
      parseJson(
        requireSuccess(runGh(["api", branchEndpoint]), "Final GitHub source branch inspection"),
        "Final GitHub source branch inspection",
      ),
    ) !== canonicalPlan.expectedHeadCommit
  ) {
    throw new Error(
      "GitHub source branch HEAD changed while the Environment baseline was applied.",
    );
  }

  return {
    schemaVersion: STAGE6_ENVIRONMENT_BASELINE_APPLICATION_SCHEMA,
    repository: canonicalPlan.repository,
    sourceBranch: canonicalPlan.sourceBranch,
    expectedHeadCommit: canonicalPlan.expectedHeadCommit,
    environment: ENVIRONMENT,
    planSha256,
    reviewers: canonicalPlan.reviewers,
    environmentExistedBeforeApply,
    changed,
    administratorBypassDisabled: after.administratorBypassDisabled,
    manualAdministratorBypassActionRequired: !after.administratorBypassDisabled,
    verification: {
      repositoryAdminAuthorityReadBeforeWrite: true,
      exactBranchHeadReadBeforeWrite: true,
      sourceBranchProtectionReadBeforeWrite: true,
      environmentReadBeforeWrite: true,
      environmentReadAfterWrite: true,
      exactReviewersReadAfterWrite: true,
      protectedBranchesOnlyReadAfterWrite: true,
      branchHeadStableAfterWrite: true,
      administratorBypassWriteAvailableInGitHubApi: false,
    },
    assessment: after.administratorBypassDisabled
      ? "environment-baseline-ready-not-release-approved"
      : "environment-baseline-applied-manual-admin-bypass-required",
  };
}
