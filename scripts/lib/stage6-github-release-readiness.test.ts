import { describe, expect, it } from "vitest";

import {
  STAGE6_CANDIDATE_ENVIRONMENT_SECRETS,
  STAGE6_CANDIDATE_ENVIRONMENT_VARIABLES,
  STAGE6_OPTIONAL_REPOSITORY_VARIABLES,
  STAGE6_STATIC_REPOSITORY_SECRETS,
  STAGE6_STATIC_REPOSITORY_VARIABLES,
  collectStage6GitHubReleaseReadiness,
  type Stage6GitHubCommandResult,
} from "./stage6-github-release-readiness.ts";

function success(value: unknown): Stage6GitHubCommandResult {
  return { status: 0, stdout: JSON.stringify(value), stderr: "" };
}

function names(values: ReadonlyArray<string>) {
  return values.map((name) => ({ name }));
}

const workflow = [
  ...STAGE6_STATIC_REPOSITORY_SECRETS.map((name) => `secrets.${name}`),
  ...STAGE6_CANDIDATE_ENVIRONMENT_SECRETS.map((name) => `secrets.${name}`),
  ...STAGE6_STATIC_REPOSITORY_VARIABLES.map((name) => `vars.${name}`),
  ...STAGE6_CANDIDATE_ENVIRONMENT_VARIABLES.map((name) => `vars.${name}`),
  ...STAGE6_OPTIONAL_REPOSITORY_VARIABLES.map((name) => `vars.${name}`),
].join("\n");

const branchProtection = {
  enforce_admins: { enabled: true },
  required_pull_request_reviews: {
    dismiss_stale_reviews: true,
    required_approving_review_count: 1,
  },
  required_status_checks: { strict: true, checks: [{ context: "CI" }] },
  allow_force_pushes: { enabled: false },
  allow_deletions: { enabled: false },
};

const environment = {
  name: "stage6-enterprise-ga",
  can_admins_bypass: false,
  deployment_branch_policy: { protected_branches: true, custom_branch_policies: false },
  protection_rules: [
    {
      type: "required_reviewers",
      prevent_self_review: true,
      reviewers: [
        { type: "User", reviewer: { id: 101 } },
        { type: "Team", reviewer: { id: 202 } },
      ],
    },
  ],
};

function readyRunner(ownerType: "User" | "Organization" = "User") {
  return (args: ReadonlyArray<string>): Stage6GitHubCommandResult => {
    const command = args.join(" ");
    if (command === "api repos/synara-ai/synara") {
      return success({ owner: { type: ownerType } });
    }
    if (command === "secret list -R synara-ai/synara --json name") {
      return success(names(STAGE6_STATIC_REPOSITORY_SECRETS));
    }
    if (command === "variable list -R synara-ai/synara --json name") {
      return success(names([...STAGE6_STATIC_REPOSITORY_VARIABLES, "SYNARA_PUBLISH_CLI"]));
    }
    if (command === "variable get SYNARA_FINALIZE_RELEASE -R synara-ai/synara --json name,value") {
      return success({ name: "SYNARA_FINALIZE_RELEASE", value: "1" });
    }
    if (command === "api repos/synara-ai/synara/environments/stage6-enterprise-ga") {
      return success(environment);
    }
    if (command === "secret list -R synara-ai/synara --env stage6-enterprise-ga --json name") {
      return success(names(STAGE6_CANDIDATE_ENVIRONMENT_SECRETS));
    }
    if (command === "variable list -R synara-ai/synara --env stage6-enterprise-ga --json name") {
      return success(names(STAGE6_CANDIDATE_ENVIRONMENT_VARIABLES));
    }
    if (command === "api repos/synara-ai/synara/branches/main/protection") {
      return success(branchProtection);
    }
    if (command.startsWith("run list -R synara-ai/synara --workflow release.yml")) {
      return success([{ databaseId: 123 }]);
    }
    throw new Error(`Unexpected GitHub command: ${command}`);
  };
}

describe("Stage 6 GitHub release readiness", () => {
  it("projects a fully configured user-owned repository without reading secret values", () => {
    const readiness = collectStage6GitHubReleaseReadiness(
      {
        repository: "synara-ai/synara",
        sourceBranch: "main",
        workflowPath: "unused.yml",
      },
      { runGh: readyRunner(), readWorkflow: () => `${workflow}\n${workflow}` },
    );

    expect(readiness.readyForCandidateEnvironmentApply).toBe(true);
    expect(readiness.readyForProtectedCandidateDispatch).toBe(true);
    expect(readiness.assessment).toBe("github-release-inputs-ready-not-ga-approved");
    expect(readiness.inputs.missingRepositorySecretNames).toEqual([]);
    expect(readiness.inputs.invalidRepositoryVariableNames).toEqual([]);
    expect(readiness.inputs.disallowedGaRepositoryVariableNames).toEqual([]);
    expect(JSON.stringify(readiness)).not.toContain("secret-value");
  });

  it("records a missing environment and branch protection without querying environment inputs", () => {
    const commands: string[] = [];
    const runGh = (args: ReadonlyArray<string>): Stage6GitHubCommandResult => {
      const command = args.join(" ");
      commands.push(command);
      if (command === "api repos/synara-ai/synara") {
        return success({ owner: { type: "User" } });
      }
      if (command === "secret list -R synara-ai/synara --json name") return success([]);
      if (command === "variable list -R synara-ai/synara --json name") return success([]);
      if (command.includes("/environments/") || command.includes("/protection")) {
        return { status: 1, stdout: "", stderr: "gh: Not Found (HTTP 404)" };
      }
      if (command.startsWith("run list ")) return success([]);
      throw new Error(`Unexpected GitHub command: ${command}`);
    };

    const readiness = collectStage6GitHubReleaseReadiness(
      {
        repository: "synara-ai/synara",
        sourceBranch: "main",
        workflowPath: "unused.yml",
      },
      { runGh, readWorkflow: () => workflow },
    );

    expect(readiness.controls.environmentConfigured).toBe(false);
    expect(readiness.controls.sourceBranchProtected).toBe(false);
    expect(readiness.readyForCandidateEnvironmentApply).toBe(false);
    expect(readiness.releaseHistory.hasReleaseRun).toBe(false);
    expect(commands.some((command) => command.includes("--env"))).toBe(false);
    expect(commands.some((command) => command.startsWith("variable get "))).toBe(false);
  });

  it("rejects a present but disabled finalization variable without exposing its value", () => {
    const base = readyRunner();
    const readiness = collectStage6GitHubReleaseReadiness(
      {
        repository: "synara-ai/synara",
        sourceBranch: "main",
        workflowPath: "unused.yml",
      },
      {
        readWorkflow: () => workflow,
        runGh: (args) => {
          const command = args.join(" ");
          if (
            command === "variable get SYNARA_FINALIZE_RELEASE -R synara-ai/synara --json name,value"
          ) {
            return success({ name: "SYNARA_FINALIZE_RELEASE", value: "0" });
          }
          return base(args);
        },
      },
    );

    expect(readiness.inputs.invalidRepositoryVariableNames).toEqual(["SYNARA_FINALIZE_RELEASE"]);
    expect(readiness.readyForCandidateEnvironmentApply).toBe(false);
    expect(readiness.readyForProtectedCandidateDispatch).toBe(false);
    expect(JSON.stringify(readiness)).not.toContain('"value":"0"');
  });

  it("rejects any configured unsigned-Windows exception for Enterprise GA", () => {
    const base = readyRunner();
    const readiness = collectStage6GitHubReleaseReadiness(
      {
        repository: "synara-ai/synara",
        sourceBranch: "main",
        workflowPath: "unused.yml",
      },
      {
        readWorkflow: () => workflow,
        runGh: (args) => {
          const command = args.join(" ");
          if (command === "variable list -R synara-ai/synara --json name") {
            return success(
              names([
                ...STAGE6_STATIC_REPOSITORY_VARIABLES,
                ...STAGE6_OPTIONAL_REPOSITORY_VARIABLES,
              ]),
            );
          }
          return base(args);
        },
      },
    );

    expect(readiness.inputs.disallowedGaRepositoryVariableNames).toEqual([
      "SYNARA_ALLOW_UNSIGNED_WINDOWS_RELEASE",
    ]);
    expect(readiness.readyForCandidateEnvironmentApply).toBe(false);
    expect(readiness.readyForProtectedCandidateDispatch).toBe(false);
  });

  it("allows applying the protected environment from its safe baseline before candidate inputs exist", () => {
    const baselineEnvironment = {
      ...environment,
      deployment_branch_policy: {
        protected_branches: false,
        custom_branch_policies: true,
      },
      protection_rules: [],
    };
    const base = readyRunner();
    const readiness = collectStage6GitHubReleaseReadiness(
      {
        repository: "synara-ai/synara",
        sourceBranch: "main",
        workflowPath: "unused.yml",
      },
      {
        readWorkflow: () => workflow,
        runGh: (args) => {
          const command = args.join(" ");
          if (command === "api repos/synara-ai/synara/environments/stage6-enterprise-ga") {
            return success(baselineEnvironment);
          }
          if (
            command === "secret list -R synara-ai/synara --env stage6-enterprise-ga --json name"
          ) {
            return success([]);
          }
          if (
            command === "variable list -R synara-ai/synara --env stage6-enterprise-ga --json name"
          ) {
            return success([]);
          }
          return base(args);
        },
      },
    );

    expect(readiness.readyForCandidateEnvironmentApply).toBe(true);
    expect(readiness.readyForProtectedCandidateDispatch).toBe(false);
    expect(readiness.controls.preventSelfReview).toBe(false);
    expect(readiness.controls.protectedBranchesOnly).toBe(false);
  });

  it("does not relabel authorization failures as absent configuration", () => {
    const runGh = (args: ReadonlyArray<string>): Stage6GitHubCommandResult => {
      const command = args.join(" ");
      if (command === "api repos/synara-ai/synara") {
        return success({ owner: { type: "User" } });
      }
      if (command === "secret list -R synara-ai/synara --json name") return success([]);
      if (command === "variable list -R synara-ai/synara --json name") return success([]);
      return { status: 1, stdout: "", stderr: "gh: Resource not accessible (HTTP 403)" };
    };

    expect(() =>
      collectStage6GitHubReleaseReadiness(
        {
          repository: "synara-ai/synara",
          sourceBranch: "main",
          workflowPath: "unused.yml",
        },
        { runGh, readWorkflow: () => workflow },
      ),
    ).toThrow("HTTP 403");
  });

  it("keeps organization-scoped secret visibility and workflow drift fail closed", () => {
    const readiness = collectStage6GitHubReleaseReadiness(
      {
        repository: "synara-ai/synara",
        sourceBranch: "main",
        workflowPath: "unused.yml",
      },
      {
        runGh: readyRunner("Organization"),
        readWorkflow: () => `${workflow}\nsecrets.UNCLASSIFIED_SIGNER`,
      },
    );

    expect(readiness.inputs.organizationSecretVisibilityNotAssessed).toBe(true);
    expect(readiness.workflowContract.exactKnownSecretReferences).toBe(false);
    expect(readiness.readyForCandidateEnvironmentApply).toBe(false);
  });

  it("URL-encodes source branches and rejects malformed reviewer identities", () => {
    const commands: string[] = [];
    const malformedEnvironment = {
      ...environment,
      protection_rules: [
        {
          type: "required_reviewers",
          prevent_self_review: true,
          reviewers: [
            { type: "User", reviewer: { id: 101 } },
            { type: "Robot", reviewer: { id: "202" } },
          ],
        },
      ],
    };
    const base = readyRunner();
    const readiness = collectStage6GitHubReleaseReadiness(
      {
        repository: "synara-ai/synara",
        sourceBranch: "release/stage-6",
        workflowPath: "unused.yml",
      },
      {
        readWorkflow: () => workflow,
        runGh: (args) => {
          const command = args.join(" ");
          commands.push(command);
          if (command === "api repos/synara-ai/synara/environments/stage6-enterprise-ga") {
            return success(malformedEnvironment);
          }
          if (command === "api repos/synara-ai/synara/branches/release%2Fstage-6/protection") {
            return success(branchProtection);
          }
          return base(args);
        },
      },
    );

    expect(commands).toContain("api repos/synara-ai/synara/branches/release%2Fstage-6/protection");
    expect(readiness.controls.separatedReviewerCount).toBe(0);
    expect(readiness.readyForCandidateEnvironmentApply).toBe(true);
    expect(readiness.readyForProtectedCandidateDispatch).toBe(false);
  });
});
