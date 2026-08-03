import { mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";

import {
  assertVerifiedStage6EnvironmentApproval,
  verifyStage6EnvironmentApproval,
} from "./stage6-environment-approval.ts";

const roots: string[] = [];
const sourceCommit = "a".repeat(40);

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { force: true, recursive: true });
});

function fixture() {
  const root = mkdtempSync(join(tmpdir(), "synara-stage6-environment-approval-"));
  roots.push(root);
  const environmentPath = join(root, "environment.json");
  const approvalsPath = join(root, "approvals.json");
  const workflowRunPath = join(root, "workflow-run.json");
  const branchProtectionPath = join(root, "branch-protection.json");
  const environment = {
    id: 12345,
    name: "stage6-enterprise-ga",
    url: "https://api.github.com/repos/synara-ai/synara/environments/stage6-enterprise-ga",
    html_url:
      "https://github.com/synara-ai/synara/deployments/activity_log?environments_filter=stage6-enterprise-ga",
    can_admins_bypass: false,
    deployment_branch_policy: {
      protected_branches: true,
      custom_branch_policies: false,
    },
    protection_rules: [
      {
        id: 44,
        type: "required_reviewers",
        prevent_self_review: true,
        reviewers: [
          {
            type: "User",
            reviewer: { id: 101, node_id: "USER_releasemanager", login: "release-manager" },
          },
          {
            type: "Team",
            reviewer: { id: 202, node_id: "TEAM_security", slug: "security" },
          },
        ],
      },
      { id: 45, type: "branch_policy" },
    ],
  };
  const approvals = [
    {
      state: "approved",
      comment: "SYNARA_STAGE6_APPROVE v1.2.3 987654321",
      environments: [{ id: 12345, name: "stage6-enterprise-ga" }],
      user: {
        id: 303,
        node_id: "USER_actualreviewer",
        login: "security-reviewer",
        type: "User",
      },
    },
  ];
  const workflowRun = {
    id: 987654321,
    run_attempt: 1,
    event: "workflow_dispatch",
    path: ".github/workflows/release.yml",
    head_sha: sourceCommit,
    head_branch: "main",
    status: "in_progress",
    repository: { full_name: "synara-ai/synara" },
  };
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
  const write = (): void => {
    writeFileSync(environmentPath, `${JSON.stringify(environment)}\n`);
    writeFileSync(approvalsPath, `${JSON.stringify(approvals)}\n`);
    writeFileSync(workflowRunPath, `${JSON.stringify(workflowRun)}\n`);
    writeFileSync(branchProtectionPath, `${JSON.stringify(branchProtection)}\n`);
  };
  write();
  return {
    root,
    environmentPath,
    approvalsPath,
    workflowRunPath,
    branchProtectionPath,
    environment,
    approvals,
    workflowRun,
    branchProtection,
    write,
    input: {
      environmentResponsePath: environmentPath,
      approvalHistoryResponsePath: approvalsPath,
      workflowRunResponsePath: workflowRunPath,
      branchProtectionResponsePath: branchProtectionPath,
      repository: "synara-ai/synara",
      environmentName: "stage6-enterprise-ga" as const,
      runId: "987654321",
      runAttempt: 1,
      expectedSourceCommit: sourceCommit,
      actor: "release-operator",
      triggeringActor: "release-operator",
      expectedReviewComment: "SYNARA_STAGE6_APPROVE v1.2.3 987654321",
    },
  };
}

describe("Stage 6 GitHub environment approval evidence", () => {
  it("binds the actual reviewer, current protection policy, actors and first workflow attempt", () => {
    const value = fixture();
    const verified = verifyStage6EnvironmentApproval(value.input);
    expect(verified.environment).toMatchObject({
      id: 12345,
      preventSelfReview: true,
      administratorBypassDisabled: true,
      protectedBranchesOnly: true,
    });
    expect(verified.review.reviewer.login).toBe("security-reviewer");
    expect(verified.workflow).toMatchObject({ runId: "987654321", runAttempt: 1 });
    expect(verified.approvalEvidenceSha256).toMatch(/^sha256:[0-9a-f]{64}$/);
    expect(() => assertVerifiedStage6EnvironmentApproval(verified)).not.toThrow();
    expect(() => assertVerifiedStage6EnvironmentApproval({ ...verified })).toThrow(
      "was not produced by the GitHub evidence verifier",
    );
  });

  it("rejects administrator bypass, missing reviewer separation and rerun history reuse", () => {
    const bypass = fixture();
    bypass.environment.can_admins_bypass = true;
    bypass.write();
    expect(() => verifyStage6EnvironmentApproval(bypass.input)).toThrow("administrator");

    const oneReviewer = fixture();
    oneReviewer.environment.protection_rules[0]!.reviewers!.pop();
    oneReviewer.write();
    expect(() => verifyStage6EnvironmentApproval(oneReviewer.input)).toThrow("two and six");

    const rerun = fixture();
    expect(() => verifyStage6EnvironmentApproval({ ...rerun.input, runAttempt: 2 })).toThrow(
      "fresh first-attempt",
    );
  });

  it("rejects self review, a generic comment, or ambiguous approval history", () => {
    const selfReview = fixture();
    selfReview.approvals[0]!.user.login = selfReview.input.actor;
    selfReview.write();
    expect(() => verifyStage6EnvironmentApproval(selfReview.input)).toThrow("differ");

    const comment = fixture();
    comment.approvals[0]!.comment = "Ship it";
    comment.write();
    expect(() => verifyStage6EnvironmentApproval(comment.input)).toThrow("comment");

    const duplicate = fixture();
    duplicate.approvals.push(structuredClone(duplicate.approvals[0]!));
    duplicate.write();
    expect(() => verifyStage6EnvironmentApproval(duplicate.input)).toThrow("exactly one");
  });

  it("rejects broad branch access or an extra deployment protection rule", () => {
    const broadBranches = fixture();
    broadBranches.environment.deployment_branch_policy.protected_branches = false;
    broadBranches.environment.deployment_branch_policy.custom_branch_policies = true;
    broadBranches.write();
    expect(() => verifyStage6EnvironmentApproval(broadBranches.input)).toThrow(
      "protected branches only",
    );

    const customRule = fixture();
    customRule.environment.protection_rules.push({ id: 46, type: "custom" } as never);
    customRule.write();
    expect(() => verifyStage6EnvironmentApproval(customRule.input)).toThrow("unapproved");
  });

  it("rejects a different workflow source or weakened source-branch protection", () => {
    const workflowDrift = fixture();
    workflowDrift.workflowRun.head_sha = "f".repeat(40);
    workflowDrift.write();
    expect(() => verifyStage6EnvironmentApproval(workflowDrift.input)).toThrow("source commit");

    const weakBranch = fixture();
    weakBranch.branchProtection.enforce_admins.enabled = false;
    weakBranch.write();
    expect(() => verifyStage6EnvironmentApproval(weakBranch.input)).toThrow(
      "must enforce administrators",
    );
  });

  it("rejects a symlinked GitHub response", () => {
    const value = fixture();
    const symlink = join(value.root, "environment-link.json");
    symlinkSync(value.environmentPath, symlink);
    expect(() =>
      verifyStage6EnvironmentApproval({ ...value.input, environmentResponsePath: symlink }),
    ).toThrow("regular non-symlink");
  });
});
