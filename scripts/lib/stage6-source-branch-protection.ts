import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";

import { verifyStage6SourceBranchProtectionResponse } from "./stage6-protected-environment-apply.ts";

const REPOSITORY_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const BRANCH_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,198}[A-Za-z0-9._-])?$/;
const COMMIT_PATTERN = /^[0-9a-f]{40}$/;
const CHECK_PATTERN = /^[^\u0000-\u001f\u007f]{1,200}$/;
const MAX_GH_OUTPUT_BYTES = 2 * 1024 * 1024;

export const STAGE6_SOURCE_BRANCH_PROTECTION_PLAN_SCHEMA =
  "synara.stage6-source-branch-protection-plan.v1";
export const STAGE6_SOURCE_BRANCH_PROTECTION_APPLICATION_SCHEMA =
  "synara.stage6-source-branch-protection-application.v1";

export interface Stage6SourceBranchProtectionCommandResult {
  readonly status: number;
  readonly stdout: string;
  readonly stderr: string;
}

export interface Stage6SourceBranchProtectionPlan {
  readonly schemaVersion: typeof STAGE6_SOURCE_BRANCH_PROTECTION_PLAN_SCHEMA;
  readonly repository: string;
  readonly branch: string;
  readonly expectedHeadCommit: string;
  readonly requiredStatusChecks: ReadonlyArray<string>;
  readonly policy: {
    readonly enforceAdministrators: true;
    readonly dismissStaleReviews: true;
    readonly requiredApprovingReviewCount: 1;
    readonly requireLastPushApproval: true;
    readonly requireStrictStatusChecks: true;
    readonly requireConversationResolution: true;
    readonly requireLinearHistory: true;
    readonly allowForcePushes: false;
    readonly allowDeletions: false;
  };
  readonly assessment: "configuration-planned-not-applied";
}

export interface Stage6SourceBranchProtectionApplicationReceipt {
  readonly schemaVersion: typeof STAGE6_SOURCE_BRANCH_PROTECTION_APPLICATION_SCHEMA;
  readonly repository: string;
  readonly branch: string;
  readonly expectedHeadCommit: string;
  readonly planSha256: string;
  readonly requiredStatusChecks: ReadonlyArray<string>;
  readonly previousProtectionConfigured: boolean;
  readonly previousProtectionAlreadyCompliant: boolean;
  readonly changed: boolean;
  readonly verification: {
    readonly repositoryAdminAuthorityReadBeforeWrite: true;
    readonly exactBranchHeadReadBeforeWrite: true;
    readonly previousProtectionReadBeforeWrite: true;
    readonly exactPolicyReadAfterWrite: true;
    readonly branchHeadStableAfterWrite: true;
  };
  readonly assessment: "configuration-applied-not-release-approved";
}

export interface Stage6SourceBranchProtectionDependencies {
  readonly runGh?: (
    args: ReadonlyArray<string>,
    stdin?: string,
  ) => Stage6SourceBranchProtectionCommandResult;
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
): Stage6SourceBranchProtectionCommandResult {
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

function requireSuccess(result: Stage6SourceBranchProtectionCommandResult, label: string): string {
  if (result.status !== 0) {
    throw new Error(
      `${label} failed with exit status ${result.status}: ${(result.stderr || result.stdout).trim()}`,
    );
  }
  return result.stdout;
}

function validateIdentity(input: {
  readonly repository: string;
  readonly branch: string;
  readonly expectedHeadCommit: string;
}): void {
  if (!REPOSITORY_PATTERN.test(input.repository)) throw new Error("GitHub repository is invalid.");
  if (
    !BRANCH_PATTERN.test(input.branch) ||
    input.branch.includes("..") ||
    input.branch.includes("//")
  ) {
    throw new Error("GitHub source branch is invalid.");
  }
  if (!COMMIT_PATTERN.test(input.expectedHeadCommit)) {
    throw new Error("Expected GitHub source branch HEAD commit is invalid.");
  }
}

function normalizeChecks(checks: ReadonlyArray<string>): ReadonlyArray<string> {
  if (checks.length === 0 || checks.length > 20) {
    throw new Error("Stage 6 source branch requires between one and twenty status checks.");
  }
  const normalized = checks.map((check) => {
    if (check !== check.trim() || !CHECK_PATTERN.test(check)) {
      throw new Error("Stage 6 required status check is invalid.");
    }
    return check;
  });
  const unique = [...new Set(normalized)].sort();
  if (unique.length !== normalized.length) {
    throw new Error("Stage 6 required status checks must be unique.");
  }
  return Object.freeze(unique);
}

export function createStage6SourceBranchProtectionPlan(input: {
  readonly repository: string;
  readonly branch: string;
  readonly expectedHeadCommit: string;
  readonly requiredStatusChecks: ReadonlyArray<string>;
}): Stage6SourceBranchProtectionPlan {
  validateIdentity(input);
  return Object.freeze({
    schemaVersion: STAGE6_SOURCE_BRANCH_PROTECTION_PLAN_SCHEMA,
    repository: input.repository,
    branch: input.branch,
    expectedHeadCommit: input.expectedHeadCommit,
    requiredStatusChecks: normalizeChecks(input.requiredStatusChecks),
    policy: Object.freeze({
      enforceAdministrators: true,
      dismissStaleReviews: true,
      requiredApprovingReviewCount: 1,
      requireLastPushApproval: true,
      requireStrictStatusChecks: true,
      requireConversationResolution: true,
      requireLinearHistory: true,
      allowForcePushes: false,
      allowDeletions: false,
    }),
    assessment: "configuration-planned-not-applied",
  });
}

export function hashStage6SourceBranchProtectionPlan(
  plan: Stage6SourceBranchProtectionPlan,
): string {
  return `sha256:${createHash("sha256").update(JSON.stringify(plan)).digest("hex")}`;
}

function extractStatusChecks(protection: Record<string, unknown>): ReadonlyArray<string> {
  const statusChecks = requireObject(
    protection.required_status_checks,
    "GitHub required status checks",
  );
  const checks = Array.isArray(statusChecks.checks) ? statusChecks.checks : undefined;
  if (checks) {
    return checks.map((entry) => {
      const check = requireObject(entry, "GitHub required status check");
      if (typeof check.context !== "string") {
        throw new Error("GitHub required status check context is invalid.");
      }
      return check.context;
    });
  }
  if (Array.isArray(statusChecks.contexts)) {
    return statusChecks.contexts.map((entry) => {
      if (typeof entry !== "string") {
        throw new Error("GitHub required status check context is invalid.");
      }
      return entry;
    });
  }
  return [];
}

function requireEnabledRule(
  protection: Record<string, unknown>,
  name: string,
  label: string,
): void {
  const rule = requireObject(protection[name], label);
  if (rule.enabled !== true) throw new Error(`${label} must be enabled.`);
}

export function verifyStage6SourceBranchProtectionExact(
  raw: unknown,
  requiredStatusChecks: ReadonlyArray<string>,
): void {
  verifyStage6SourceBranchProtectionResponse(raw);
  const protection = requireObject(raw, "GitHub source branch protection response");
  const pullRequests = requireObject(
    protection.required_pull_request_reviews,
    "GitHub required pull-request reviews",
  );
  if (pullRequests.require_last_push_approval !== true) {
    throw new Error("Stage 6 source branch must require approval after the latest push.");
  }
  requireEnabledRule(
    protection,
    "required_conversation_resolution",
    "GitHub conversation-resolution rule",
  );
  requireEnabledRule(protection, "required_linear_history", "GitHub linear-history rule");
  const expected = [...normalizeChecks(requiredStatusChecks)].sort();
  const actual = [...extractStatusChecks(protection)].sort();
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    throw new Error("Stage 6 source branch required status checks do not match the plan.");
  }
}

function branchHead(raw: unknown): string {
  const branch = requireObject(raw, "GitHub branch response");
  const commit = requireObject(branch.commit, "GitHub branch commit");
  if (typeof commit.sha !== "string" || !COMMIT_PATTERN.test(commit.sha)) {
    throw new Error("GitHub branch response has an invalid HEAD commit.");
  }
  return commit.sha;
}

type Stage6RepositoryOwnerType = "User" | "Organization";

function requireRepositoryAdmin(raw: unknown, repository: string): Stage6RepositoryOwnerType {
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
  const owner = requireObject(response.owner, "GitHub repository owner");
  if (owner.type !== "User" && owner.type !== "Organization") {
    throw new Error("GitHub repository owner type is invalid.");
  }
  return owner.type;
}

function protectionPayload(
  checks: ReadonlyArray<string>,
  ownerType: Stage6RepositoryOwnerType,
): string {
  return JSON.stringify({
    required_status_checks: { strict: true, contexts: checks },
    enforce_admins: true,
    required_pull_request_reviews: {
      dismiss_stale_reviews: true,
      require_code_owner_reviews: false,
      required_approving_review_count: 1,
      require_last_push_approval: true,
      // GitHub rejects bypass allowances on non-organization repositories (HTTP 422).
      ...(ownerType === "Organization"
        ? { bypass_pull_request_allowances: { users: [], teams: [], apps: [] } }
        : {}),
    },
    restrictions: null,
    required_linear_history: true,
    allow_force_pushes: false,
    allow_deletions: false,
    block_creations: false,
    required_conversation_resolution: true,
    lock_branch: false,
    allow_fork_syncing: false,
  });
}

function enabled(protection: Record<string, unknown>, name: string): boolean {
  const value = protection[name];
  return Boolean(
    value &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    (value as Record<string, unknown>).enabled === true,
  );
}

function assertNoStrongerControlsWouldBeRemoved(
  raw: unknown,
  plannedStatusChecks: ReadonlyArray<string>,
): void {
  const protection = requireObject(raw, "Existing GitHub source branch protection response");
  const planned = new Set(plannedStatusChecks);
  const unplannedChecks = extractStatusChecks(protection).filter((check) => !planned.has(check));
  const pullRequests = requireObject(
    protection.required_pull_request_reviews,
    "Existing GitHub required pull-request reviews",
  );
  const restrictions = protection.restrictions;
  const hasRestrictions =
    restrictions !== null &&
    restrictions !== undefined &&
    Object.values(requireObject(restrictions, "Existing GitHub push restrictions")).some(
      (value) => Array.isArray(value) && value.length > 0,
    );
  if (
    unplannedChecks.length > 0 ||
    (typeof pullRequests.required_approving_review_count === "number" &&
      pullRequests.required_approving_review_count > 1) ||
    pullRequests.require_code_owner_reviews === true ||
    hasRestrictions ||
    enabled(protection, "required_signatures") ||
    enabled(protection, "lock_branch") ||
    enabled(protection, "block_creations")
  ) {
    throw new Error(
      "Existing GitHub source branch protection contains stronger or additional controls that this plan would remove.",
    );
  }
}

function isNotFound(result: Stage6SourceBranchProtectionCommandResult): boolean {
  return result.status !== 0 && /(?:HTTP\s+404|Not Found)/i.test(result.stderr || result.stdout);
}

export function applyStage6SourceBranchProtection(
  input: {
    readonly plan: Stage6SourceBranchProtectionPlan;
    readonly confirmationSha256: string;
  },
  dependencies: Stage6SourceBranchProtectionDependencies = {},
): Stage6SourceBranchProtectionApplicationReceipt {
  const canonicalPlan = createStage6SourceBranchProtectionPlan(input.plan);
  if (
    input.plan.schemaVersion !== STAGE6_SOURCE_BRANCH_PROTECTION_PLAN_SCHEMA ||
    input.plan.assessment !== "configuration-planned-not-applied" ||
    JSON.stringify(input.plan) !== JSON.stringify(canonicalPlan)
  ) {
    throw new Error("Stage 6 source branch protection plan is not canonical.");
  }
  const planSha256 = hashStage6SourceBranchProtectionPlan(canonicalPlan);
  if (input.confirmationSha256 !== planSha256) {
    throw new Error("Stage 6 source branch protection confirmation digest does not match.");
  }
  const runGh = dependencies.runGh ?? defaultRunGh;
  const repositoryText = requireSuccess(
    runGh(["api", `repos/${canonicalPlan.repository}`]),
    "GitHub repository inspection",
  );
  const repositoryOwnerType = requireRepositoryAdmin(
    parseJson(repositoryText, "GitHub repository inspection"),
    canonicalPlan.repository,
  );
  const branchEndpoint = `repos/${canonicalPlan.repository}/branches/${encodeURIComponent(canonicalPlan.branch)}`;
  const beforeBranchText = requireSuccess(
    runGh(["api", branchEndpoint]),
    "GitHub source branch inspection",
  );
  if (
    branchHead(parseJson(beforeBranchText, "GitHub source branch inspection")) !==
    canonicalPlan.expectedHeadCommit
  ) {
    throw new Error("GitHub source branch HEAD differs from the confirmed plan.");
  }

  const protectionEndpoint = `${branchEndpoint}/protection`;
  const beforeProtection = runGh(["api", protectionEndpoint]);
  let previousProtectionConfigured = false;
  let previousProtectionAlreadyCompliant = false;
  let previousProtectionValue: unknown;
  if (beforeProtection.status === 0) {
    previousProtectionConfigured = true;
    previousProtectionValue = parseJson(
      beforeProtection.stdout,
      "GitHub source branch protection inspection",
    );
    try {
      verifyStage6SourceBranchProtectionExact(
        previousProtectionValue,
        canonicalPlan.requiredStatusChecks,
      );
      previousProtectionAlreadyCompliant = true;
    } catch {
      previousProtectionAlreadyCompliant = false;
    }
  } else if (!isNotFound(beforeProtection)) {
    requireSuccess(beforeProtection, "GitHub source branch protection inspection");
  }

  if (!previousProtectionAlreadyCompliant) {
    if (previousProtectionConfigured) {
      assertNoStrongerControlsWouldBeRemoved(
        previousProtectionValue,
        canonicalPlan.requiredStatusChecks,
      );
    }
    const appliedText = requireSuccess(
      runGh(
        ["api", "--method", "PUT", protectionEndpoint, "--input", "-"],
        protectionPayload(canonicalPlan.requiredStatusChecks, repositoryOwnerType),
      ),
      "GitHub source branch protection update",
    );
    verifyStage6SourceBranchProtectionExact(
      parseJson(appliedText, "GitHub source branch protection update"),
      canonicalPlan.requiredStatusChecks,
    );
  }

  const afterProtectionText = requireSuccess(
    runGh(["api", protectionEndpoint]),
    "Final GitHub source branch protection inspection",
  );
  verifyStage6SourceBranchProtectionExact(
    parseJson(afterProtectionText, "Final GitHub source branch protection inspection"),
    canonicalPlan.requiredStatusChecks,
  );
  const afterBranchText = requireSuccess(
    runGh(["api", branchEndpoint]),
    "Final GitHub source branch inspection",
  );
  if (
    branchHead(parseJson(afterBranchText, "Final GitHub source branch inspection")) !==
    canonicalPlan.expectedHeadCommit
  ) {
    throw new Error("GitHub source branch HEAD changed while protection was applied.");
  }

  return {
    schemaVersion: STAGE6_SOURCE_BRANCH_PROTECTION_APPLICATION_SCHEMA,
    repository: canonicalPlan.repository,
    branch: canonicalPlan.branch,
    expectedHeadCommit: canonicalPlan.expectedHeadCommit,
    planSha256,
    requiredStatusChecks: canonicalPlan.requiredStatusChecks,
    previousProtectionConfigured,
    previousProtectionAlreadyCompliant,
    changed: !previousProtectionAlreadyCompliant,
    verification: {
      repositoryAdminAuthorityReadBeforeWrite: true,
      exactBranchHeadReadBeforeWrite: true,
      previousProtectionReadBeforeWrite: true,
      exactPolicyReadAfterWrite: true,
      branchHeadStableAfterWrite: true,
    },
    assessment: "configuration-applied-not-release-approved",
  };
}
