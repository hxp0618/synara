import { createHash } from "node:crypto";
import { closeSync, constants, fstatSync, lstatSync, openSync, readSync } from "node:fs";
import { TextDecoder } from "node:util";

import {
  verifyStage6SourceBranchProtectionResponse,
  verifyStage6WorkflowRunResponse,
} from "./stage6-protected-environment-apply.ts";

const MAX_GITHUB_RESPONSE_BYTES = 2 * 1024 * 1024;
const REPOSITORY_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const LOGIN_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$/;
const TEAM_SLUG_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,98}[A-Za-z0-9])?$/;
const NODE_ID_PATTERN = /^[A-Za-z0-9_+=/-]{2,240}$/;

const VERIFIED_STAGE6_ENVIRONMENT_APPROVAL: unique symbol = Symbol(
  "VerifiedStage6EnvironmentApproval",
);
const VERIFIED_STAGE6_ENVIRONMENT_APPROVALS = new WeakSet<object>();

export const STAGE6_ENVIRONMENT_APPROVAL_SCHEMA = "synara.stage6-github-environment-approval.v2";

export interface Stage6EnvironmentApprovalInput {
  readonly environmentResponsePath: string;
  readonly approvalHistoryResponsePath: string;
  readonly workflowRunResponsePath: string;
  readonly branchProtectionResponsePath: string;
  readonly repository: string;
  readonly environmentName: "stage6-enterprise-ga";
  readonly runId: string;
  readonly runAttempt: number;
  readonly expectedSourceCommit: string;
  readonly actor: string;
  readonly triggeringActor: string;
  readonly expectedReviewComment: string;
}

export interface VerifiedStage6EnvironmentApproval {
  readonly schemaVersion: typeof STAGE6_ENVIRONMENT_APPROVAL_SCHEMA;
  readonly environment: {
    readonly id: number;
    readonly name: "stage6-enterprise-ga";
    readonly apiUrl: string;
    readonly htmlUrl: string;
    readonly configuredReviewers: ReadonlyArray<{
      readonly type: "User" | "Team";
      readonly id: number;
      readonly nodeId: string;
      readonly loginOrSlug: string;
    }>;
    readonly preventSelfReview: true;
    readonly administratorBypassDisabled: true;
    readonly protectedBranchesOnly: true;
    readonly configurationSha256: string;
  };
  readonly workflow: {
    readonly runId: string;
    readonly runAttempt: 1;
    readonly sourceCommit: string;
    readonly sourceBranch: string;
    readonly actor: string;
    readonly triggeringActor: string;
  };
  readonly review: {
    readonly state: "approved";
    readonly reviewer: {
      readonly type: "User";
      readonly id: number;
      readonly nodeId: string;
      readonly login: string;
    };
    readonly commentSha256: string;
    readonly reviewSha256: string;
  };
  readonly approvalEvidenceSha256: string;
  readonly [VERIFIED_STAGE6_ENVIRONMENT_APPROVAL]: true;
}

export function assertVerifiedStage6EnvironmentApproval(
  value: unknown,
): asserts value is VerifiedStage6EnvironmentApproval {
  if (
    typeof value !== "object" ||
    value === null ||
    !VERIFIED_STAGE6_ENVIRONMENT_APPROVALS.has(value)
  ) {
    throw new Error(
      "Stage 6 environment approval was not produced by the GitHub evidence verifier.",
    );
  }
}

function sha256(value: string | Buffer): string {
  return `sha256:${createHash("sha256").update(value).digest("hex")}`;
}

function requireObject(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object.`);
  }
  return value as Record<string, unknown>;
}

function requireString(value: unknown, label: string): string {
  if (typeof value !== "string" || value.length === 0 || /[\r\n\t]/.test(value)) {
    throw new Error(`${label} must be a non-empty single-line string.`);
  }
  return value;
}

function requirePositiveId(value: unknown, label: string): number {
  if (!Number.isSafeInteger(value) || (value as number) <= 0) {
    throw new Error(`${label} must be a positive safe integer.`);
  }
  return value as number;
}

function readJsonResponse(path: string, label: string): unknown {
  const entry = lstatSync(path);
  if (!entry.isFile() || entry.isSymbolicLink() || entry.size <= 0) {
    throw new Error(`${label} must be a non-empty regular non-symlink file.`);
  }
  if (entry.size > MAX_GITHUB_RESPONSE_BYTES) {
    throw new Error(`${label} exceeds the bounded GitHub response size.`);
  }
  const descriptor = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const opened = fstatSync(descriptor);
    if (
      !opened.isFile() ||
      opened.dev !== entry.dev ||
      opened.ino !== entry.ino ||
      opened.size !== entry.size
    ) {
      throw new Error(`${label} changed while it was opened.`);
    }
    const bytes = Buffer.allocUnsafe(opened.size);
    let offset = 0;
    while (offset < bytes.byteLength) {
      const bytesRead = readSync(descriptor, bytes, offset, bytes.byteLength - offset, offset);
      if (bytesRead === 0) throw new Error(`${label} changed while it was read.`);
      offset += bytesRead;
    }
    const after = fstatSync(descriptor);
    if (
      after.size !== opened.size ||
      after.mtimeMs !== opened.mtimeMs ||
      after.ctimeMs !== opened.ctimeMs
    ) {
      throw new Error(`${label} changed while it was read.`);
    }
    try {
      return JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
    } catch (error) {
      throw new Error(`${label} must contain valid UTF-8 JSON.`, { cause: error });
    }
  } finally {
    closeSync(descriptor);
  }
}

function requireGitHubUrl(
  value: unknown,
  host: "api.github.com" | "github.com",
  label: string,
  allowSearch = false,
): string {
  const raw = requireString(value, label);
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch (error) {
    throw new Error(`${label} is not a valid URL.`, { cause: error });
  }
  if (
    parsed.protocol !== "https:" ||
    parsed.hostname !== host ||
    parsed.username !== "" ||
    parsed.password !== "" ||
    (!allowSearch && parsed.search !== "") ||
    parsed.hash !== ""
  ) {
    throw new Error(`${label} must be a credential-free GitHub HTTPS URL.`);
  }
  return parsed.toString();
}

function requireLogin(value: unknown, label: string): string {
  const login = requireString(value, label);
  if (!LOGIN_PATTERN.test(login)) throw new Error(`${label} is invalid.`);
  return login;
}

function requireTeamSlug(value: unknown, label: string): string {
  const slug = requireString(value, label);
  if (!TEAM_SLUG_PATTERN.test(slug)) throw new Error(`${label} is invalid.`);
  return slug;
}

function requireNodeId(value: unknown, label: string): string {
  const nodeId = requireString(value, label);
  if (!NODE_ID_PATTERN.test(nodeId)) throw new Error(`${label} is invalid.`);
  return nodeId;
}

export function verifyStage6EnvironmentApproval(
  input: Stage6EnvironmentApprovalInput,
): VerifiedStage6EnvironmentApproval {
  if (!REPOSITORY_PATTERN.test(input.repository)) throw new Error("GitHub repository is invalid.");
  if (!/^[1-9][0-9]{0,19}$/.test(input.runId)) throw new Error("Workflow run ID is invalid.");
  if (input.runAttempt !== 1) {
    throw new Error(
      "Stage 6 Enterprise GA publication requires a fresh first-attempt workflow run; reruns cannot reuse review history.",
    );
  }
  const actor = requireLogin(input.actor, "Workflow actor");
  const triggeringActor = requireLogin(input.triggeringActor, "Workflow triggering actor");
  const expectedReviewComment = requireString(
    input.expectedReviewComment,
    "Expected deployment review comment",
  );
  if (!/^[0-9a-f]{40}$/.test(input.expectedSourceCommit)) {
    throw new Error("Expected workflow source commit is invalid.");
  }
  const sourceBranch = verifyStage6WorkflowRunResponse(
    readJsonResponse(input.workflowRunResponsePath, "GitHub workflow run response"),
    {
      repository: input.repository,
      runId: input.runId,
      sourceCommit: input.expectedSourceCommit,
    },
  );
  verifyStage6SourceBranchProtectionResponse(
    readJsonResponse(input.branchProtectionResponsePath, "GitHub branch protection response"),
  );

  const environment = requireObject(
    readJsonResponse(input.environmentResponsePath, "GitHub environment response"),
    "GitHub environment response",
  );
  const environmentId = requirePositiveId(environment.id, "GitHub environment ID");
  if (environment.name !== input.environmentName) {
    throw new Error("GitHub environment response does not identify stage6-enterprise-ga.");
  }
  const apiUrl = requireGitHubUrl(environment.url, "api.github.com", "GitHub environment API URL");
  const htmlUrl = requireGitHubUrl(
    environment.html_url,
    "github.com",
    "GitHub environment HTML URL",
    true,
  );
  const expectedApiPath = `/repos/${input.repository}/environments/${input.environmentName}`;
  if (new URL(apiUrl).pathname !== expectedApiPath) {
    throw new Error("GitHub environment API URL does not match the release repository.");
  }
  const parsedHtmlUrl = new URL(htmlUrl);
  if (
    parsedHtmlUrl.pathname !== `/${input.repository}/deployments/activity_log` ||
    parsedHtmlUrl.searchParams.size !== 1 ||
    parsedHtmlUrl.searchParams.get("environments_filter") !== input.environmentName
  ) {
    throw new Error("GitHub environment HTML URL does not match the release repository.");
  }
  if (environment.can_admins_bypass !== false) {
    throw new Error("Stage 6 environment must disable administrator protection-rule bypass.");
  }
  if (!Array.isArray(environment.protection_rules)) {
    throw new Error("GitHub environment protection rules must be an array.");
  }
  const deploymentBranchPolicy = requireObject(
    environment.deployment_branch_policy,
    "GitHub environment deployment branch policy",
  );
  if (
    deploymentBranchPolicy.protected_branches !== true ||
    deploymentBranchPolicy.custom_branch_policies !== false
  ) {
    throw new Error("Stage 6 environment must allow protected branches only.");
  }
  const protectionRules = environment.protection_rules.map((rawRule) =>
    requireObject(rawRule, "GitHub environment protection rule"),
  );
  const unexpectedRules = protectionRules.filter(
    (rule) => rule.type !== "required_reviewers" && rule.type !== "branch_policy",
  );
  if (unexpectedRules.length > 0) {
    throw new Error("Stage 6 environment contains an unapproved deployment protection rule.");
  }
  const branchPolicyRules = protectionRules.filter((rule) => rule.type === "branch_policy");
  if (branchPolicyRules.length !== 1) {
    throw new Error("Stage 6 environment must have exactly one protected-branch rule.");
  }
  const reviewerRules = protectionRules.filter((rule) => rule.type === "required_reviewers");
  if (reviewerRules.length !== 1) {
    throw new Error("Stage 6 environment must have exactly one required-reviewers rule.");
  }
  const reviewerRule = requireObject(reviewerRules[0], "GitHub required-reviewers rule");
  if (reviewerRule.prevent_self_review !== true) {
    throw new Error("Stage 6 environment must prevent self-review.");
  }
  if (!Array.isArray(reviewerRule.reviewers)) {
    throw new Error("GitHub required-reviewers rule must list reviewers.");
  }
  if (reviewerRule.reviewers.length < 2 || reviewerRule.reviewers.length > 6) {
    throw new Error("Stage 6 environment must configure between two and six reviewers.");
  }
  const configuredReviewerKeys = new Set<string>();
  const configuredReviewers = reviewerRule.reviewers
    .map((rawReviewer, index) => {
      const wrapper = requireObject(rawReviewer, `Configured reviewer ${index}`);
      if (wrapper.type !== "User" && wrapper.type !== "Team") {
        throw new Error(`Configured reviewer ${index} has an unsupported type.`);
      }
      const reviewer = requireObject(wrapper.reviewer, `Configured reviewer ${index} identity`);
      const id = requirePositiveId(reviewer.id, `Configured reviewer ${index} ID`);
      const nodeId = requireNodeId(reviewer.node_id, `Configured reviewer ${index} node ID`);
      const loginOrSlug =
        wrapper.type === "User"
          ? requireLogin(reviewer.login, `Configured reviewer ${index} login`)
          : requireTeamSlug(reviewer.slug, `Configured reviewer ${index} slug`);
      const key = `${wrapper.type}:${id}`;
      if (configuredReviewerKeys.has(key)) throw new Error("Configured reviewers must be unique.");
      configuredReviewerKeys.add(key);
      return Object.freeze({ type: wrapper.type, id, nodeId, loginOrSlug });
    })
    .toSorted((left, right) =>
      `${left.type}:${left.id}`.localeCompare(`${right.type}:${right.id}`),
    );

  const history = readJsonResponse(
    input.approvalHistoryResponsePath,
    "GitHub review history response",
  );
  if (!Array.isArray(history)) throw new Error("GitHub review history response must be an array.");
  const matchingApprovals = history.filter((rawReview) => {
    const review = requireObject(rawReview, "GitHub deployment review");
    if (review.state !== "approved") return false;
    if (!Array.isArray(review.environments)) {
      throw new Error("GitHub approved review must identify its environments.");
    }
    return review.environments.some((rawEnvironment) => {
      const reviewedEnvironment = requireObject(rawEnvironment, "GitHub reviewed environment");
      return (
        reviewedEnvironment.id === environmentId &&
        reviewedEnvironment.name === input.environmentName
      );
    });
  });
  if (matchingApprovals.length !== 1) {
    throw new Error(
      "Expected exactly one approved review for this Stage 6 environment and workflow run.",
    );
  }
  const review = requireObject(matchingApprovals[0], "GitHub Stage 6 deployment review");
  const reviewer = requireObject(review.user, "GitHub Stage 6 deployment reviewer");
  if (reviewer.type !== "User")
    throw new Error("Stage 6 deployment reviewer must be a GitHub User.");
  const reviewerLogin = requireLogin(reviewer.login, "Stage 6 deployment reviewer login");
  const reviewerId = requirePositiveId(reviewer.id, "Stage 6 deployment reviewer ID");
  const reviewerNodeId = requireNodeId(reviewer.node_id, "Stage 6 deployment reviewer node ID");
  if (
    !configuredReviewers.some((configured) => configured.type === "Team") &&
    !configuredReviewers.some(
      (configured) => configured.type === "User" && configured.id === reviewerId,
    )
  ) {
    throw new Error("Stage 6 deployment reviewer is not one of the configured required reviewers.");
  }
  if (
    reviewerLogin.toLowerCase() === actor.toLowerCase() ||
    reviewerLogin.toLowerCase() === triggeringActor.toLowerCase()
  ) {
    throw new Error("Stage 6 deployment reviewer must differ from both workflow actors.");
  }
  if (review.comment !== expectedReviewComment) {
    throw new Error("Stage 6 deployment review comment does not bind this exact candidate run.");
  }

  const environmentConfiguration = {
    id: environmentId,
    name: input.environmentName,
    apiUrl,
    htmlUrl,
    configuredReviewers: Object.freeze(configuredReviewers),
    preventSelfReview: true as const,
    administratorBypassDisabled: true as const,
    protectedBranchesOnly: true as const,
  };
  const configurationSha256 = sha256(JSON.stringify(environmentConfiguration));
  const reviewEvidence = {
    state: "approved" as const,
    reviewer: {
      type: "User" as const,
      id: reviewerId,
      nodeId: reviewerNodeId,
      login: reviewerLogin,
    },
    commentSha256: sha256(expectedReviewComment),
  };
  const reviewSha256 = sha256(
    JSON.stringify({
      ...reviewEvidence,
      environmentId,
      runId: input.runId,
      runAttempt: input.runAttempt,
    }),
  );
  const canonical = {
    schemaVersion: STAGE6_ENVIRONMENT_APPROVAL_SCHEMA,
    environment: Object.freeze({ ...environmentConfiguration, configurationSha256 }),
    workflow: Object.freeze({
      runId: input.runId,
      runAttempt: 1 as const,
      sourceCommit: input.expectedSourceCommit,
      sourceBranch,
      actor,
      triggeringActor,
    }),
    review: Object.freeze({
      ...reviewEvidence,
      reviewer: Object.freeze(reviewEvidence.reviewer),
      reviewSha256,
    }),
  } as const;
  const verified: VerifiedStage6EnvironmentApproval = Object.freeze({
    ...canonical,
    approvalEvidenceSha256: sha256(JSON.stringify(canonical)),
    [VERIFIED_STAGE6_ENVIRONMENT_APPROVAL]: true as const,
  });
  VERIFIED_STAGE6_ENVIRONMENT_APPROVALS.add(verified);
  return verified;
}
