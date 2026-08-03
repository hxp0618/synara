// FILE: stage6-github-release-readiness.ts
// Purpose: Reads GitHub release controls without mutating settings or exposing secret values.
// Layer: Stage 6 release tooling

import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { verifyStage6SourceBranchProtectionResponse } from "./stage6-protected-environment-apply.ts";

const REPOSITORY_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const BRANCH_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,198}[A-Za-z0-9._-])?$/;
const MAX_GH_OUTPUT_BYTES = 2 * 1024 * 1024;
export const STAGE6_GITHUB_ENVIRONMENT = "stage6-enterprise-ga";

export const STAGE6_STATIC_REPOSITORY_SECRETS = [
  "APPLE_API_ISSUER",
  "APPLE_API_KEY",
  "APPLE_API_KEY_ID",
  "APPLE_TEAM_ID",
  "AZURE_CLIENT_ID",
  "AZURE_CLIENT_SECRET",
  "AZURE_TENANT_ID",
  "AZURE_TRUSTED_SIGNING_ACCOUNT_NAME",
  "AZURE_TRUSTED_SIGNING_CERTIFICATE_PROFILE_NAME",
  "AZURE_TRUSTED_SIGNING_ENDPOINT",
  "AZURE_TRUSTED_SIGNING_PUBLISHER_NAME",
  "AZURE_TRUSTED_SIGNING_SUBJECT_DN",
  "CSC_KEY_PASSWORD",
  "CSC_LINK",
  "RELEASE_APP_ID",
  "RELEASE_APP_PRIVATE_KEY",
] as const;

export const STAGE6_CANDIDATE_ENVIRONMENT_SECRETS = [
  "SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64",
] as const;

export const STAGE6_STATIC_REPOSITORY_VARIABLES = ["SYNARA_FINALIZE_RELEASE"] as const;
export const STAGE6_STATIC_REPOSITORY_VARIABLE_VALUES = {
  SYNARA_FINALIZE_RELEASE: "1",
} as const;

export const STAGE6_CANDIDATE_ENVIRONMENT_VARIABLES = [
  "SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID",
  "SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_SHA256",
  "SYNARA_STAGE6_CANDIDATE_ID",
  "SYNARA_STAGE6_CANDIDATE_SOURCE_COMMIT",
  "SYNARA_STAGE6_DESKTOP_ARTIFACT_SET_SHA256",
] as const;

export const STAGE6_OPTIONAL_REPOSITORY_VARIABLES = [
  "SYNARA_ALLOW_UNSIGNED_WINDOWS_RELEASE",
  "SYNARA_PUBLISH_CLI",
] as const;
export const STAGE6_DISALLOWED_GA_REPOSITORY_VARIABLES = [
  "SYNARA_ALLOW_UNSIGNED_WINDOWS_RELEASE",
] as const;

export interface Stage6GitHubCommandResult {
  readonly status: number;
  readonly stdout: string;
  readonly stderr: string;
}

export interface Stage6GitHubReadinessDependencies {
  readonly runGh?: (args: ReadonlyArray<string>) => Stage6GitHubCommandResult;
  readonly readWorkflow?: (path: string) => string;
}

export interface Stage6GitHubReleaseReadiness {
  readonly schemaVersion: "synara.stage6-github-release-readiness.v1";
  readonly repository: string;
  readonly sourceBranch: string;
  readonly environment: typeof STAGE6_GITHUB_ENVIRONMENT;
  readonly repositoryOwnerType: "User" | "Organization";
  readonly workflowContract: {
    readonly exactKnownSecretReferences: boolean;
    readonly exactKnownVariableReferences: boolean;
    readonly referencedSecretNames: ReadonlyArray<string>;
    readonly referencedVariableNames: ReadonlyArray<string>;
  };
  readonly controls: {
    readonly sourceBranchProtected: boolean;
    readonly environmentConfigured: boolean;
    readonly administratorBypassDisabled: boolean;
    readonly preventSelfReview: boolean;
    readonly protectedBranchesOnly: boolean;
    readonly separatedReviewerCount: number;
  };
  readonly inputs: {
    readonly missingRepositorySecretNames: ReadonlyArray<string>;
    readonly missingRepositoryVariableNames: ReadonlyArray<string>;
    readonly invalidRepositoryVariableNames: ReadonlyArray<string>;
    readonly disallowedGaRepositoryVariableNames: ReadonlyArray<string>;
    readonly missingCandidateEnvironmentSecretNames: ReadonlyArray<string>;
    readonly missingCandidateEnvironmentVariableNames: ReadonlyArray<string>;
    readonly configuredOptionalRepositoryVariableNames: ReadonlyArray<string>;
    readonly organizationSecretVisibilityNotAssessed: boolean;
  };
  readonly releaseHistory: {
    readonly runCountObserved: number;
    readonly hasReleaseRun: boolean;
  };
  readonly readyForCandidateEnvironmentApply: boolean;
  readonly readyForProtectedCandidateDispatch: boolean;
  readonly assessment:
    | "github-release-inputs-ready-not-ga-approved"
    | "github-release-inputs-incomplete";
}

function defaultRunGh(args: ReadonlyArray<string>): Stage6GitHubCommandResult {
  const result = spawnSync("gh", [...args], {
    encoding: "utf8",
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

function parseJson(text: string, label: string): unknown {
  try {
    return JSON.parse(text);
  } catch (error) {
    throw new Error(`${label} returned invalid JSON.`, { cause: error });
  }
}

function requireObject(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object.`);
  }
  return value as Record<string, unknown>;
}

function runRequired(
  runGh: (args: ReadonlyArray<string>) => Stage6GitHubCommandResult,
  args: ReadonlyArray<string>,
  label: string,
): string {
  const result = runGh(args);
  if (result.status !== 0) {
    throw new Error(`${label} failed: ${(result.stderr || result.stdout).trim()}`);
  }
  return result.stdout;
}

function runOptional404(
  runGh: (args: ReadonlyArray<string>) => Stage6GitHubCommandResult,
  args: ReadonlyArray<string>,
  label: string,
): string | null {
  const result = runGh(args);
  if (result.status === 0) return result.stdout;
  const error = `${result.stderr}\n${result.stdout}`;
  if (/\bHTTP 404\b|\bstatus["']?\s*:\s*404\b|\bnot found\b/i.test(error)) return null;
  throw new Error(`${label} failed: ${(result.stderr || result.stdout).trim()}`);
}

function nameList(text: string, label: string): ReadonlyArray<string> {
  const value = parseJson(text, label);
  if (!Array.isArray(value)) throw new Error(`${label} must be an array.`);
  const names = value.map((entry, index) => {
    const object = requireObject(entry, `${label}[${index}]`);
    if (typeof object.name !== "string" || !/^[A-Z][A-Z0-9_]{0,127}$/.test(object.name)) {
      throw new Error(`${label}[${index}].name is invalid.`);
    }
    return object.name;
  });
  return [...new Set(names)].toSorted();
}

function extractWorkflowNames(workflow: string, kind: "secrets" | "vars"): ReadonlyArray<string> {
  const pattern = new RegExp(`\\b${kind}\\.([A-Z][A-Z0-9_]*)`, "g");
  return [...new Set([...workflow.matchAll(pattern)].map((match) => match[1]!))].toSorted();
}

function missing(required: ReadonlyArray<string>, configured: ReadonlyArray<string>) {
  const configuredSet = new Set(configured);
  return required.filter((name) => !configuredSet.has(name));
}

function exactSet(left: ReadonlyArray<string>, right: ReadonlyArray<string>): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

function assessEnvironment(raw: unknown): Stage6GitHubReleaseReadiness["controls"] {
  if (raw === null) {
    return {
      sourceBranchProtected: false,
      environmentConfigured: false,
      administratorBypassDisabled: false,
      preventSelfReview: false,
      protectedBranchesOnly: false,
      separatedReviewerCount: 0,
    };
  }
  const environment = requireObject(raw, "GitHub Environment");
  const branchPolicy = requireObject(
    environment.deployment_branch_policy,
    "GitHub Environment branch policy",
  );
  const rules = Array.isArray(environment.protection_rules) ? environment.protection_rules : [];
  const reviewerRule = rules
    .map((rule, index) => requireObject(rule, `GitHub Environment rule ${index}`))
    .find((rule) => rule.type === "required_reviewers");
  const reviewers =
    reviewerRule && Array.isArray(reviewerRule.reviewers) ? reviewerRule.reviewers : [];
  const reviewerIds = new Set<string>();
  let reviewersValid = true;
  for (const [index, reviewer] of reviewers.entries()) {
    const wrapper = requireObject(reviewer, `GitHub Environment reviewer ${index}`);
    const entity = requireObject(wrapper.reviewer, `GitHub Environment reviewer ${index} identity`);
    if (
      (wrapper.type !== "User" && wrapper.type !== "Team") ||
      !Number.isSafeInteger(entity.id) ||
      (entity.id as number) <= 0
    ) {
      reviewersValid = false;
      continue;
    }
    reviewerIds.add(`${wrapper.type}:${entity.id as number}`);
  }
  return {
    sourceBranchProtected: false,
    environmentConfigured: environment.name === STAGE6_GITHUB_ENVIRONMENT,
    administratorBypassDisabled: environment.can_admins_bypass === false,
    preventSelfReview: reviewerRule?.prevent_self_review === true,
    protectedBranchesOnly:
      branchPolicy.protected_branches === true && branchPolicy.custom_branch_policies === false,
    separatedReviewerCount: reviewersValid ? reviewerIds.size : 0,
  };
}

export function collectStage6GitHubReleaseReadiness(
  input: {
    readonly repository: string;
    readonly sourceBranch: string;
    readonly workflowPath: string;
  },
  dependencies: Stage6GitHubReadinessDependencies = {},
): Stage6GitHubReleaseReadiness {
  if (!REPOSITORY_PATTERN.test(input.repository)) throw new Error("GitHub repository is invalid.");
  if (!BRANCH_PATTERN.test(input.sourceBranch) || input.sourceBranch.includes("..")) {
    throw new Error("GitHub source branch is invalid.");
  }
  const runGh = dependencies.runGh ?? defaultRunGh;
  const workflow = (dependencies.readWorkflow ?? ((path) => readFileSync(resolve(path), "utf8")))(
    input.workflowPath,
  );
  const referencedSecrets = extractWorkflowNames(workflow, "secrets");
  const referencedVariables = extractWorkflowNames(workflow, "vars");
  const knownSecrets = [
    ...STAGE6_STATIC_REPOSITORY_SECRETS,
    ...STAGE6_CANDIDATE_ENVIRONMENT_SECRETS,
  ].toSorted();
  const knownVariables = [
    ...STAGE6_STATIC_REPOSITORY_VARIABLES,
    ...STAGE6_CANDIDATE_ENVIRONMENT_VARIABLES,
    ...STAGE6_OPTIONAL_REPOSITORY_VARIABLES,
  ].toSorted();

  const repository = requireObject(
    parseJson(
      runRequired(runGh, ["api", `repos/${input.repository}`], "GitHub repository inspection"),
      "GitHub repository inspection",
    ),
    "GitHub repository inspection",
  );
  const owner = requireObject(repository.owner, "GitHub repository owner");
  if (owner.type !== "User" && owner.type !== "Organization") {
    throw new Error("GitHub repository owner type is invalid.");
  }

  const repositorySecrets = nameList(
    runRequired(
      runGh,
      ["secret", "list", "-R", input.repository, "--json", "name"],
      "GitHub repository secret-name inspection",
    ),
    "GitHub repository secret-name inspection",
  );
  const repositoryVariables = nameList(
    runRequired(
      runGh,
      ["variable", "list", "-R", input.repository, "--json", "name"],
      "GitHub repository variable-name inspection",
    ),
    "GitHub repository variable-name inspection",
  );
  const repositoryVariableValues = new Map<string, string>();
  for (const name of STAGE6_STATIC_REPOSITORY_VARIABLES) {
    if (!repositoryVariables.includes(name)) continue;
    const variable = requireObject(
      parseJson(
        runRequired(
          runGh,
          ["variable", "get", name, "-R", input.repository, "--json", "name,value"],
          `GitHub repository variable value inspection (${name})`,
        ),
        `GitHub repository variable value inspection (${name})`,
      ),
      `GitHub repository variable value inspection (${name})`,
    );
    if (variable.name !== name || typeof variable.value !== "string") {
      throw new Error(`GitHub repository variable ${name} response is invalid.`);
    }
    repositoryVariableValues.set(name, variable.value);
  }
  const environmentText = runOptional404(
    runGh,
    ["api", `repos/${input.repository}/environments/${STAGE6_GITHUB_ENVIRONMENT}`],
    "GitHub Environment inspection",
  );
  const environment = environmentText
    ? parseJson(environmentText, "GitHub Environment inspection")
    : null;
  const environmentSecrets = environment
    ? nameList(
        runRequired(
          runGh,
          [
            "secret",
            "list",
            "-R",
            input.repository,
            "--env",
            STAGE6_GITHUB_ENVIRONMENT,
            "--json",
            "name",
          ],
          "GitHub Environment secret-name inspection",
        ),
        "GitHub Environment secret-name inspection",
      )
    : [];
  const environmentVariables = environment
    ? nameList(
        runRequired(
          runGh,
          [
            "variable",
            "list",
            "-R",
            input.repository,
            "--env",
            STAGE6_GITHUB_ENVIRONMENT,
            "--json",
            "name",
          ],
          "GitHub Environment variable-name inspection",
        ),
        "GitHub Environment variable-name inspection",
      )
    : [];

  const protectionText = runOptional404(
    runGh,
    [
      "api",
      `repos/${input.repository}/branches/${encodeURIComponent(input.sourceBranch)}/protection`,
    ],
    "GitHub source-branch protection inspection",
  );
  let sourceBranchProtected = false;
  if (protectionText) {
    const protection = parseJson(protectionText, "GitHub source-branch protection inspection");
    try {
      verifyStage6SourceBranchProtectionResponse(protection);
      sourceBranchProtected = true;
    } catch {
      sourceBranchProtected = false;
    }
  }
  const controls = {
    ...assessEnvironment(environment),
    sourceBranchProtected,
  };

  const runs = parseJson(
    runRequired(
      runGh,
      [
        "run",
        "list",
        "-R",
        input.repository,
        "--workflow",
        "release.yml",
        "--limit",
        "10",
        "--json",
        "databaseId,status,conclusion,headSha,createdAt,url",
      ],
      "GitHub release-run inspection",
    ),
    "GitHub release-run inspection",
  );
  if (!Array.isArray(runs)) throw new Error("GitHub release-run inspection must be an array.");

  const missingRepositorySecrets = missing(STAGE6_STATIC_REPOSITORY_SECRETS, repositorySecrets);
  const missingRepositoryVariables = missing(
    STAGE6_STATIC_REPOSITORY_VARIABLES,
    repositoryVariables,
  );
  const invalidRepositoryVariables = STAGE6_STATIC_REPOSITORY_VARIABLES.filter(
    (name) =>
      repositoryVariables.includes(name) &&
      repositoryVariableValues.get(name) !== STAGE6_STATIC_REPOSITORY_VARIABLE_VALUES[name],
  );
  const disallowedGaRepositoryVariables = STAGE6_DISALLOWED_GA_REPOSITORY_VARIABLES.filter((name) =>
    repositoryVariables.includes(name),
  );
  const missingEnvironmentSecrets = missing(
    STAGE6_CANDIDATE_ENVIRONMENT_SECRETS,
    environmentSecrets,
  );
  const missingEnvironmentVariables = missing(
    STAGE6_CANDIDATE_ENVIRONMENT_VARIABLES,
    environmentVariables,
  );
  const workflowSecretContractValid = exactSet(referencedSecrets, knownSecrets);
  const workflowVariableContractValid = exactSet(referencedVariables, knownVariables);
  const organizationSecretsNotAssessed = owner.type === "Organization";
  const environmentProtectionReady =
    controls.environmentConfigured &&
    controls.administratorBypassDisabled &&
    controls.preventSelfReview &&
    controls.protectedBranchesOnly &&
    controls.separatedReviewerCount >= 2 &&
    controls.separatedReviewerCount <= 6;
  const environmentApplyBaselineReady =
    controls.environmentConfigured && controls.administratorBypassDisabled;
  const readyForApply =
    workflowSecretContractValid &&
    workflowVariableContractValid &&
    !organizationSecretsNotAssessed &&
    controls.sourceBranchProtected &&
    environmentApplyBaselineReady &&
    missingRepositorySecrets.length === 0 &&
    missingRepositoryVariables.length === 0 &&
    invalidRepositoryVariables.length === 0 &&
    disallowedGaRepositoryVariables.length === 0;
  const readyForDispatch =
    readyForApply &&
    environmentProtectionReady &&
    missingEnvironmentSecrets.length === 0 &&
    missingEnvironmentVariables.length === 0;

  return {
    schemaVersion: "synara.stage6-github-release-readiness.v1",
    repository: input.repository,
    sourceBranch: input.sourceBranch,
    environment: STAGE6_GITHUB_ENVIRONMENT,
    repositoryOwnerType: owner.type,
    workflowContract: {
      exactKnownSecretReferences: workflowSecretContractValid,
      exactKnownVariableReferences: workflowVariableContractValid,
      referencedSecretNames: referencedSecrets,
      referencedVariableNames: referencedVariables,
    },
    controls,
    inputs: {
      missingRepositorySecretNames: missingRepositorySecrets,
      missingRepositoryVariableNames: missingRepositoryVariables,
      invalidRepositoryVariableNames: invalidRepositoryVariables,
      disallowedGaRepositoryVariableNames: disallowedGaRepositoryVariables,
      missingCandidateEnvironmentSecretNames: missingEnvironmentSecrets,
      missingCandidateEnvironmentVariableNames: missingEnvironmentVariables,
      configuredOptionalRepositoryVariableNames: STAGE6_OPTIONAL_REPOSITORY_VARIABLES.filter(
        (name) => repositoryVariables.includes(name),
      ),
      organizationSecretVisibilityNotAssessed: organizationSecretsNotAssessed,
    },
    releaseHistory: {
      runCountObserved: runs.length,
      hasReleaseRun: runs.length > 0,
    },
    readyForCandidateEnvironmentApply: readyForApply,
    readyForProtectedCandidateDispatch: readyForDispatch,
    assessment: readyForDispatch
      ? "github-release-inputs-ready-not-ga-approved"
      : "github-release-inputs-incomplete",
  };
}
