import { describe, expect, it } from "vitest";

import {
  applyStage6SourceBranchProtection,
  createStage6SourceBranchProtectionPlan,
  hashStage6SourceBranchProtectionPlan,
  type Stage6SourceBranchProtectionCommandResult,
} from "./stage6-source-branch-protection.ts";

const repository = "synara-ai/synara";
const branch = "release/stage-6";
const expectedHeadCommit = "a".repeat(40);
const requiredStatusChecks = ["CI / Go Control Plane", "CI / Release Smoke"];

function success(value: unknown): Stage6SourceBranchProtectionCommandResult {
  return { status: 0, stdout: JSON.stringify(value), stderr: "" };
}

function repositoryResponse(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    full_name: repository,
    archived: false,
    disabled: false,
    permissions: { admin: true },
    owner: { type: "Organization" },
    ...overrides,
  };
}

function protection(checks = requiredStatusChecks): Record<string, unknown> {
  return {
    enforce_admins: { enabled: true },
    required_pull_request_reviews: {
      dismiss_stale_reviews: true,
      required_approving_review_count: 1,
      require_last_push_approval: true,
    },
    required_status_checks: {
      strict: true,
      checks: checks.map((context) => ({ context })),
    },
    required_conversation_resolution: { enabled: true },
    required_linear_history: { enabled: true },
    allow_force_pushes: { enabled: false },
    allow_deletions: { enabled: false },
  };
}

function plan() {
  return createStage6SourceBranchProtectionPlan({
    repository,
    branch,
    expectedHeadCommit,
    requiredStatusChecks,
  });
}

describe("Stage 6 source branch protection", () => {
  it("creates a deterministic canonical plan without contacting GitHub", () => {
    const first = plan();
    const reordered = createStage6SourceBranchProtectionPlan({
      repository,
      branch,
      expectedHeadCommit,
      requiredStatusChecks: [...requiredStatusChecks].reverse(),
    });

    expect(reordered).toEqual(first);
    expect(hashStage6SourceBranchProtectionPlan(reordered)).toBe(
      hashStage6SourceBranchProtectionPlan(first),
    );
    expect(first.assessment).toBe("configuration-planned-not-applied");
  });

  it("applies and verifies the exact policy while the confirmed branch HEAD remains stable", () => {
    const prepared = plan();
    const calls: Array<{ args: ReadonlyArray<string>; stdin?: string }> = [];
    let protectionReads = 0;
    const receipt = applyStage6SourceBranchProtection(
      {
        plan: prepared,
        confirmationSha256: hashStage6SourceBranchProtectionPlan(prepared),
      },
      {
        runGh: (args, stdin) => {
          calls.push(stdin === undefined ? { args } : { args, stdin });
          const command = args.join(" ");
          if (command === `api repos/${repository}`) {
            return success(repositoryResponse());
          }
          if (command === `api repos/${repository}/branches/release%2Fstage-6`) {
            return success({ commit: { sha: expectedHeadCommit } });
          }
          if (command === `api repos/${repository}/branches/release%2Fstage-6/protection`) {
            protectionReads += 1;
            if (protectionReads === 1) {
              return { status: 1, stdout: "", stderr: "gh: Not Found (HTTP 404)" };
            }
            return success(protection());
          }
          if (
            command ===
            `api --method PUT repos/${repository}/branches/release%2Fstage-6/protection --input -`
          ) {
            return success(protection());
          }
          throw new Error(`Unexpected GitHub command: ${command}`);
        },
      },
    );

    expect(receipt.changed).toBe(true);
    expect(receipt.previousProtectionConfigured).toBe(false);
    const write = calls.find((call) => call.args.includes("PUT"));
    expect(write).toBeDefined();
    const writeStdin = write?.stdin;
    expect(writeStdin).toBeDefined();
    if (writeStdin === undefined) {
      throw new Error("Expected the branch-protection write payload");
    }
    const payload = JSON.parse(writeStdin) as {
      required_pull_request_reviews: Record<string, unknown>;
    };
    expect(payload).toMatchObject({
      enforce_admins: true,
      required_status_checks: { strict: true, contexts: [...requiredStatusChecks].sort() },
      required_conversation_resolution: true,
      required_linear_history: true,
      allow_force_pushes: false,
      allow_deletions: false,
    });
    expect(payload.required_pull_request_reviews.bypass_pull_request_allowances).toEqual({
      users: [],
      teams: [],
      apps: [],
    });
  });

  it("omits organization-only bypass allowances for user-owned repositories", () => {
    const prepared = plan();
    const writes: string[] = [];
    let protectionReads = 0;
    applyStage6SourceBranchProtection(
      {
        plan: prepared,
        confirmationSha256: hashStage6SourceBranchProtectionPlan(prepared),
      },
      {
        runGh: (args, stdin) => {
          const command = args.join(" ");
          if (command === `api repos/${repository}`) {
            return success(repositoryResponse({ owner: { type: "User" } }));
          }
          if (command === `api repos/${repository}/branches/release%2Fstage-6`) {
            return success({ commit: { sha: expectedHeadCommit } });
          }
          if (command === `api repos/${repository}/branches/release%2Fstage-6/protection`) {
            protectionReads += 1;
            if (protectionReads === 1) {
              return { status: 1, stdout: "", stderr: "gh: Not Found (HTTP 404)" };
            }
            return success(protection());
          }
          if (
            command ===
            `api --method PUT repos/${repository}/branches/release%2Fstage-6/protection --input -`
          ) {
            writes.push(stdin ?? "");
            return success(protection());
          }
          throw new Error(`Unexpected GitHub command: ${command}`);
        },
      },
    );

    expect(writes).toHaveLength(1);
    const [write] = writes;
    if (write === undefined) {
      throw new Error("Expected the branch-protection write payload");
    }
    const payload = JSON.parse(write) as {
      required_pull_request_reviews: Record<string, unknown>;
    };
    expect(payload.required_pull_request_reviews).toMatchObject({
      required_approving_review_count: 1,
      require_last_push_approval: true,
    });
    expect("bypass_pull_request_allowances" in payload.required_pull_request_reviews).toBe(false);
  });

  it("rejects an unknown repository owner type before any write", () => {
    const prepared = plan();
    const commands: string[] = [];
    expect(() =>
      applyStage6SourceBranchProtection(
        {
          plan: prepared,
          confirmationSha256: hashStage6SourceBranchProtectionPlan(prepared),
        },
        {
          runGh: (args) => {
            commands.push(args.join(" "));
            return success(repositoryResponse({ owner: { type: "Bot" } }));
          },
        },
      ),
    ).toThrow("owner type is invalid");
    expect(commands.some((command) => command.includes("PUT"))).toBe(false);
  });

  it("is idempotent when the exact policy is already present", () => {
    const prepared = plan();
    const commands: string[] = [];
    const receipt = applyStage6SourceBranchProtection(
      {
        plan: prepared,
        confirmationSha256: hashStage6SourceBranchProtectionPlan(prepared),
      },
      {
        runGh: (args) => {
          const command = args.join(" ");
          commands.push(command);
          if (command === `api repos/${repository}`) {
            return success(repositoryResponse());
          }
          if (command === `api repos/${repository}/branches/release%2Fstage-6`) {
            return success({ commit: { sha: expectedHeadCommit } });
          }
          if (command.endsWith("/protection")) return success(protection());
          throw new Error(`Unexpected GitHub command: ${command}`);
        },
      },
    );

    expect(receipt.changed).toBe(false);
    expect(receipt.previousProtectionAlreadyCompliant).toBe(true);
    expect(commands.some((command) => command.includes(" PUT "))).toBe(false);
  });

  it("rejects a mismatched confirmation before contacting GitHub", () => {
    let contacted = false;
    expect(() =>
      applyStage6SourceBranchProtection(
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

  it("rejects missing admin authority or branch drift before any write", () => {
    const prepared = plan();
    const confirmationSha256 = hashStage6SourceBranchProtectionPlan(prepared);
    expect(() =>
      applyStage6SourceBranchProtection(
        { plan: prepared, confirmationSha256 },
        {
          runGh: () => success(repositoryResponse({ permissions: { admin: false } })),
        },
      ),
    ).toThrow("administrator authority");

    const commands: string[] = [];
    expect(() =>
      applyStage6SourceBranchProtection(
        { plan: prepared, confirmationSha256 },
        {
          runGh: (args) => {
            const command = args.join(" ");
            commands.push(command);
            if (command === `api repos/${repository}`) {
              return success(repositoryResponse());
            }
            return success({ commit: { sha: "b".repeat(40) } });
          },
        },
      ),
    ).toThrow("HEAD differs");
    expect(commands.some((command) => command.includes("PUT"))).toBe(false);
  });

  it("does not treat authorization failure as absent protection", () => {
    const prepared = plan();
    expect(() =>
      applyStage6SourceBranchProtection(
        {
          plan: prepared,
          confirmationSha256: hashStage6SourceBranchProtectionPlan(prepared),
        },
        {
          runGh: (args) => {
            const command = args.join(" ");
            if (command === `api repos/${repository}`) {
              return success(repositoryResponse());
            }
            if (command === `api repos/${repository}/branches/release%2Fstage-6`) {
              return success({ commit: { sha: expectedHeadCommit } });
            }
            return { status: 1, stdout: "", stderr: "gh: Forbidden (HTTP 403)" };
          },
        },
      ),
    ).toThrow("HTTP 403");
  });

  it("refuses to remove stronger existing controls before writing", () => {
    const prepared = plan();
    const commands: string[] = [];
    expect(() =>
      applyStage6SourceBranchProtection(
        {
          plan: prepared,
          confirmationSha256: hashStage6SourceBranchProtectionPlan(prepared),
        },
        {
          runGh: (args) => {
            const command = args.join(" ");
            commands.push(command);
            if (command === `api repos/${repository}`) {
              return success(repositoryResponse());
            }
            if (command === `api repos/${repository}/branches/release%2Fstage-6`) {
              return success({ commit: { sha: expectedHeadCommit } });
            }
            if (command.endsWith("/protection")) {
              return success({
                ...protection([...requiredStatusChecks, "Security / Dependency Review"]),
                required_pull_request_reviews: {
                  dismiss_stale_reviews: true,
                  required_approving_review_count: 2,
                  require_last_push_approval: false,
                  require_code_owner_reviews: true,
                },
              });
            }
            throw new Error(`Unexpected GitHub command: ${command}`);
          },
        },
      ),
    ).toThrow("stronger or additional controls");
    expect(commands.some((command) => command.includes("PUT"))).toBe(false);
  });
});
