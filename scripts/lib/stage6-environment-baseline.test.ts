import { describe, expect, it } from "vitest";

import {
  applyStage6EnvironmentBaseline,
  createStage6EnvironmentBaselinePlan,
  hashStage6EnvironmentBaselinePlan,
  type Stage6EnvironmentBaselineCommandResult,
} from "./stage6-environment-baseline.ts";
import type { Stage6EnvironmentReviewer } from "./stage6-protected-environment-apply.ts";

const repository = "synara-ai/synara";
const sourceBranch = "release/stage-6";
const expectedHeadCommit = "a".repeat(40);
const reviewers: ReadonlyArray<Stage6EnvironmentReviewer> = [
  { type: "User", id: 101 },
  { type: "Team", id: 202 },
];

function success(value: unknown): Stage6EnvironmentBaselineCommandResult {
  return { status: 0, stdout: JSON.stringify(value), stderr: "" };
}

function branchProtection(): Record<string, unknown> {
  return {
    enforce_admins: { enabled: true },
    required_pull_request_reviews: {
      dismiss_stale_reviews: true,
      required_approving_review_count: 1,
    },
    required_status_checks: { strict: true, checks: [{ context: "CI / Release Smoke" }] },
    allow_force_pushes: { enabled: false },
    allow_deletions: { enabled: false },
  };
}

function environment(
  canAdminsBypass: boolean,
  expectedReviewers: ReadonlyArray<Stage6EnvironmentReviewer> = reviewers,
): Record<string, unknown> {
  return {
    name: "stage6-enterprise-ga",
    can_admins_bypass: canAdminsBypass,
    deployment_branch_policy: { protected_branches: true, custom_branch_policies: false },
    protection_rules: [
      {
        type: "required_reviewers",
        prevent_self_review: true,
        reviewers: expectedReviewers.map((reviewer) => ({
          type: reviewer.type,
          reviewer: { id: reviewer.id },
        })),
      },
    ],
  };
}

function plan() {
  return createStage6EnvironmentBaselinePlan({
    repository,
    sourceBranch,
    expectedHeadCommit,
    reviewers,
  });
}

function baseResponse(command: string): Stage6EnvironmentBaselineCommandResult | undefined {
  if (command === `api repos/${repository}`) {
    return success({
      full_name: repository,
      archived: false,
      disabled: false,
      permissions: { admin: true },
    });
  }
  if (command === `api repos/${repository}/branches/release%2Fstage-6`) {
    return success({ commit: { sha: expectedHeadCommit } });
  }
  if (command === `api repos/${repository}/branches/release%2Fstage-6/protection`) {
    return success(branchProtection());
  }
  return undefined;
}

describe("Stage 6 GitHub Environment baseline", () => {
  it("creates a deterministic reviewer-sorted plan", () => {
    const first = plan();
    const reordered = createStage6EnvironmentBaselinePlan({
      repository,
      sourceBranch,
      expectedHeadCommit,
      reviewers: reviewers.toReversed(),
    });
    expect(reordered).toEqual(first);
    expect(hashStage6EnvironmentBaselinePlan(reordered)).toBe(
      hashStage6EnvironmentBaselinePlan(first),
    );
  });

  it("creates the API-supported baseline and reports the required manual bypass action", () => {
    const prepared = plan();
    const calls: Array<{ args: ReadonlyArray<string>; stdin?: string }> = [];
    let environmentReads = 0;
    const receipt = applyStage6EnvironmentBaseline(
      {
        plan: prepared,
        confirmationSha256: hashStage6EnvironmentBaselinePlan(prepared),
      },
      {
        runGh: (args, stdin) => {
          calls.push(stdin === undefined ? { args } : { args, stdin });
          const command = args.join(" ");
          const base = baseResponse(command);
          if (base) return base;
          if (command === `api repos/${repository}/environments/stage6-enterprise-ga`) {
            environmentReads += 1;
            if (environmentReads === 1) {
              return { status: 1, stdout: "", stderr: "gh: Not Found (HTTP 404)" };
            }
            return success(environment(true));
          }
          if (
            command ===
            `api --method PUT repos/${repository}/environments/stage6-enterprise-ga --input -`
          ) {
            return success(environment(true));
          }
          throw new Error(`Unexpected GitHub command: ${command}`);
        },
      },
    );

    expect(receipt.changed).toBe(true);
    expect(receipt.environmentExistedBeforeApply).toBe(false);
    expect(receipt.administratorBypassDisabled).toBe(false);
    expect(receipt.manualAdministratorBypassActionRequired).toBe(true);
    expect(receipt.assessment).toBe("environment-baseline-applied-manual-admin-bypass-required");
    const write = calls.find((call) => call.args.includes("PUT"));
    expect(JSON.parse(write!.stdin!)).toEqual({
      wait_timer: 0,
      prevent_self_review: true,
      reviewers: [
        { type: "Team", id: 202 },
        { type: "User", id: 101 },
      ],
      deployment_branch_policy: { protected_branches: true, custom_branch_policies: false },
    });
  });

  it("is idempotent and fully ready after the manual administrator setting is disabled", () => {
    const prepared = plan();
    const commands: string[] = [];
    const receipt = applyStage6EnvironmentBaseline(
      {
        plan: prepared,
        confirmationSha256: hashStage6EnvironmentBaselinePlan(prepared),
      },
      {
        runGh: (args) => {
          const command = args.join(" ");
          commands.push(command);
          const base = baseResponse(command);
          if (base) return base;
          if (command === `api repos/${repository}/environments/stage6-enterprise-ga`) {
            return success(environment(false));
          }
          throw new Error(`Unexpected GitHub command: ${command}`);
        },
      },
    );

    expect(receipt.changed).toBe(false);
    expect(receipt.administratorBypassDisabled).toBe(true);
    expect(receipt.manualAdministratorBypassActionRequired).toBe(false);
    expect(receipt.assessment).toBe("environment-baseline-ready-not-release-approved");
    expect(commands.some((command) => command.includes("PUT"))).toBe(false);
  });

  it("rejects digest mismatch before contacting GitHub", () => {
    let contacted = false;
    expect(() =>
      applyStage6EnvironmentBaseline(
        { plan: plan(), confirmationSha256: `sha256:${"f".repeat(64)}` },
        {
          runGh: () => {
            contacted = true;
            return success({});
          },
        },
      ),
    ).toThrow("confirmation digest does not match");
    expect(contacted).toBe(false);
  });

  it("requires the exact protected source branch before reading or writing the Environment", () => {
    const prepared = plan();
    const commands: string[] = [];
    expect(() =>
      applyStage6EnvironmentBaseline(
        {
          plan: prepared,
          confirmationSha256: hashStage6EnvironmentBaselinePlan(prepared),
        },
        {
          runGh: (args) => {
            const command = args.join(" ");
            commands.push(command);
            if (command === `api repos/${repository}`) return baseResponse(command)!;
            if (command === `api repos/${repository}/branches/release%2Fstage-6`) {
              return success({ commit: { sha: "b".repeat(40) } });
            }
            throw new Error(`Unexpected GitHub command: ${command}`);
          },
        },
      ),
    ).toThrow("HEAD differs");
    expect(commands.some((command) => command.includes("environments"))).toBe(false);
  });

  it("refuses to remove an existing wait timer or different reviewers", () => {
    const prepared = plan();
    const commands: string[] = [];
    expect(() =>
      applyStage6EnvironmentBaseline(
        {
          plan: prepared,
          confirmationSha256: hashStage6EnvironmentBaselinePlan(prepared),
        },
        {
          runGh: (args) => {
            const command = args.join(" ");
            commands.push(command);
            const base = baseResponse(command);
            if (base) return base;
            if (command === `api repos/${repository}/environments/stage6-enterprise-ga`) {
              const stronger = environment(false, [
                { type: "User", id: 303 },
                { type: "Team", id: 404 },
              ]);
              stronger.protection_rules = [
                ...(stronger.protection_rules as unknown[]),
                { type: "wait_timer", wait_timer: 30 },
              ];
              return success(stronger);
            }
            throw new Error(`Unexpected GitHub command: ${command}`);
          },
        },
      ),
    ).toThrow("would remove");
    expect(commands.some((command) => command.includes("PUT"))).toBe(false);
  });

  it("reports a drifted below-minimum reviewer set as a reviewer mismatch instead of a plan-count failure", () => {
    const prepared = plan();
    const commands: string[] = [];
    expect(() =>
      applyStage6EnvironmentBaseline(
        {
          plan: prepared,
          confirmationSha256: hashStage6EnvironmentBaselinePlan(prepared),
        },
        {
          runGh: (args) => {
            const command = args.join(" ");
            commands.push(command);
            const base = baseResponse(command);
            if (base) return base;
            if (command === `api repos/${repository}/environments/stage6-enterprise-ga`) {
              return success(environment(false, [{ type: "User", id: 101 }]));
            }
            throw new Error(`Unexpected GitHub command: ${command}`);
          },
        },
      ),
    ).toThrow("would remove");
    expect(commands.some((command) => command.includes("PUT"))).toBe(false);
  });

  it("does not relabel Environment authorization failure as missing", () => {
    const prepared = plan();
    expect(() =>
      applyStage6EnvironmentBaseline(
        {
          plan: prepared,
          confirmationSha256: hashStage6EnvironmentBaselinePlan(prepared),
        },
        {
          runGh: (args) => {
            const command = args.join(" ");
            const base = baseResponse(command);
            if (base) return base;
            return { status: 1, stdout: "", stderr: "gh: Forbidden (HTTP 403)" };
          },
        },
      ),
    ).toThrow("HTTP 403");
  });
});
