import {
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import {
  type Stage6ProtectedEnvironmentConfig,
  STAGE6_PROTECTED_ENVIRONMENT_CONFIG_ASSESSMENT,
  STAGE6_PROTECTED_ENVIRONMENT_CONFIG_SCHEMA,
} from "./stage6-protected-environment-config.ts";
import {
  type Stage6EnvironmentReviewer,
  type Stage6GhCommandInput,
  STAGE6_PROTECTED_ENVIRONMENT_APPLICATION_ASSESSMENT,
  applyStage6ProtectedEnvironmentConfiguration,
  writeStage6ProtectedEnvironmentApplicationReceipt,
} from "./stage6-protected-environment-apply.ts";

const repository = "synara-ai/synara";
const candidateId = "v0.6.3-stage6.1";
const buildRunId = "123456789";
const secretValue = "c2VjcmV0LXJlY2VpcHQtYnl0ZXM=";
const reviewers: ReadonlyArray<Stage6EnvironmentReviewer> = [
  { type: "User", id: 101 },
  { type: "Team", id: 202 },
];

const config: Stage6ProtectedEnvironmentConfig = {
  schemaVersion: STAGE6_PROTECTED_ENVIRONMENT_CONFIG_SCHEMA,
  environment: "stage6-enterprise-ga",
  variables: {
    SYNARA_STAGE6_CANDIDATE_ID: candidateId,
    SYNARA_STAGE6_CANDIDATE_SOURCE_COMMIT: "a".repeat(40),
    SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID: buildRunId,
    SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_SHA256: `sha256:${"b".repeat(64)}`,
    SYNARA_STAGE6_DESKTOP_ARTIFACT_SET_SHA256: `sha256:${"c".repeat(64)}`,
  },
  secrets: {
    SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64: secretValue,
  },
  requiredReviewComment: `SYNARA_STAGE6_APPROVE ${candidateId} ${buildRunId}`,
  receipt: {
    fileName: "stage6-candidate-bundle-receipt.json",
    sizeBytes: 20,
    sha256: `sha256:${"b".repeat(64)}`,
  },
  verification: {
    candidateReleaseBindingSchema: "synara.stage6-candidate-release-binding.v2",
    exactReceiptBytesValidated: true,
    allReceiptProjectionsValidated: true,
    base64FitsGithubSecretLimit: true,
  },
  assessment: STAGE6_PROTECTED_ENVIRONMENT_CONFIG_ASSESSMENT,
};

function environmentResponse(canAdminsBypass = false): Record<string, unknown> {
  return {
    id: 987,
    name: "stage6-enterprise-ga",
    can_admins_bypass: canAdminsBypass,
    deployment_branch_policy: {
      protected_branches: true,
      custom_branch_policies: false,
    },
    protection_rules: [
      {
        type: "required_reviewers",
        prevent_self_review: true,
        reviewers: reviewers.map((reviewer) => ({
          type: reviewer.type,
          reviewer: { id: reviewer.id },
        })),
      },
      { type: "branch_policy" },
    ],
  };
}

function variableList(): ReadonlyArray<{
  readonly name: string;
  readonly value: string;
}> {
  return Object.entries(config.variables).map(([name, value]) => ({
    name,
    value,
  }));
}

function workflowRunResponse(changes: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: Number(buildRunId),
    run_attempt: 1,
    event: "workflow_dispatch",
    path: ".github/workflows/release.yml",
    head_sha: config.variables.SYNARA_STAGE6_CANDIDATE_SOURCE_COMMIT,
    head_branch: "main",
    status: "in_progress",
    repository: { full_name: repository },
    ...changes,
  };
}

function branchProtectionResponse(): Record<string, unknown> {
  return {
    enforce_admins: { enabled: true },
    required_pull_request_reviews: {
      dismiss_stale_reviews: true,
      required_approving_review_count: 1,
    },
    required_status_checks: { strict: true, checks: [{ context: "CI" }] },
    allow_force_pushes: { enabled: false },
    allow_deletions: { enabled: false },
  };
}

function apiReadResponse(endpoint: string, canAdminsBypass = false): Record<string, unknown> {
  if (endpoint.includes("/actions/runs/")) return workflowRunResponse();
  if (endpoint.includes("/branches/")) return branchProtectionResponse();
  return environmentResponse(canAdminsBypass);
}

describe("Stage 6 protected GitHub Environment application", () => {
  it("applies exact candidate values without placing the receipt secret in argv or output", () => {
    const calls: Stage6GhCommandInput[] = [];
    let apiReads = 0;
    const receipt = applyStage6ProtectedEnvironmentConfiguration(
      {
        configurationPath: "/private/stage6-environment.json",
        repository,
        reviewers,
      },
      {
        readConfiguration: () => ({
          config,
          sha256: `sha256:${"d".repeat(64)}`,
        }),
        runGh: (input) => {
          calls.push(input);
          if (input.args[0] === "api" && input.args.length === 2) {
            const endpoint = input.args[1]!;
            if (endpoint.includes("/environments/")) apiReads += 1;
            return {
              stdout: JSON.stringify(apiReadResponse(endpoint)),
              stderr: "",
            };
          }
          if (input.args[0] === "api") {
            return {
              stdout: JSON.stringify(environmentResponse()),
              stderr: "",
            };
          }
          if (input.args[0] === "variable" && input.args[1] === "list") {
            return { stdout: JSON.stringify(variableList()), stderr: "" };
          }
          if (input.args[0] === "secret" && input.args[1] === "list") {
            return {
              stdout: JSON.stringify([{ name: "SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64" }]),
              stderr: "",
            };
          }
          return { stdout: "", stderr: "" };
        },
      },
    );

    expect(apiReads).toBe(2);
    expect(calls).toHaveLength(13);
    const secretSet = calls.find((call) => call.args[0] === "secret" && call.args[1] === "set");
    expect(secretSet).toMatchObject({ stdin: secretValue, sensitive: true });
    expect(calls.flatMap((call) => call.args)).not.toContain(secretValue);
    expect(JSON.stringify(receipt)).not.toContain(secretValue);
    expect(receipt).toMatchObject({
      repository,
      candidateId,
      buildRunId,
      sourceBranch: "main",
      protection: {
        preventSelfReview: true,
        administratorBypassDisabled: true,
        protectedBranchesOnly: true,
      },
      assessment: STAGE6_PROTECTED_ENVIRONMENT_APPLICATION_ASSESSMENT,
    });

    const root = mkdtempSync(join(tmpdir(), "synara-stage6-environment-application-"));
    try {
      const output = join(root, "application-receipt.json");
      const written = writeStage6ProtectedEnvironmentApplicationReceipt(output, receipt);
      expect(statSync(output).mode & 0o777).toBe(0o600);
      expect(statSync(written.sha256Path).mode & 0o777).toBe(0o600);
      expect(readFileSync(written.sha256Path, "utf8")).toBe(
        `${written.sha256}  application-receipt.json\n`,
      );

      const blocked = join(root, "blocked-receipt.json");
      writeFileSync(`${blocked}.sha256`, "retained sidecar\n", { mode: 0o600 });
      expect(() => writeStage6ProtectedEnvironmentApplicationReceipt(blocked, receipt)).toThrow();
      expect(existsSync(blocked)).toBe(false);
      expect(readFileSync(`${blocked}.sha256`, "utf8")).toBe("retained sidecar\n");
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it("refuses all writes when administrator bypass is still enabled", () => {
    const calls: Stage6GhCommandInput[] = [];
    expect(() =>
      applyStage6ProtectedEnvironmentConfiguration(
        { configurationPath: "/private/config.json", repository, reviewers },
        {
          readConfiguration: () => ({
            config,
            sha256: `sha256:${"d".repeat(64)}`,
          }),
          runGh: (input) => {
            calls.push(input);
            const endpoint = input.args[1]!;
            return {
              stdout: JSON.stringify(
                apiReadResponse(endpoint, endpoint.includes("/environments/")),
              ),
              stderr: "",
            };
          },
        },
      ),
    ).toThrow("administrator bypass disabled");
    expect(calls).toHaveLength(3);
    expect(calls.at(-1)?.args).toEqual([
      "api",
      "repos/synara-ai/synara/environments/stage6-enterprise-ga",
    ]);
  });

  it("rejects duplicate or insufficient reviewers before reading configuration or GitHub", () => {
    let touched = false;
    const dependencies = {
      readConfiguration: () => {
        touched = true;
        return { config, sha256: `sha256:${"d".repeat(64)}` };
      },
      runGh: () => {
        touched = true;
        return { stdout: "{}", stderr: "" };
      },
    };
    expect(() =>
      applyStage6ProtectedEnvironmentConfiguration(
        {
          configurationPath: "/private/config.json",
          repository,
          reviewers: [reviewers[0]!],
        },
        dependencies,
      ),
    ).toThrow("between two and six");
    expect(() =>
      applyStage6ProtectedEnvironmentConfiguration(
        {
          configurationPath: "/private/config.json",
          repository,
          reviewers: [reviewers[0]!, reviewers[0]!],
        },
        dependencies,
      ),
    ).toThrow("must be unique");
    expect(touched).toBe(false);
  });

  it("rejects a symlinked private configuration before contacting GitHub", () => {
    const root = mkdtempSync(join(tmpdir(), "synara-stage6-environment-config-link-"));
    try {
      const target = join(root, "target.json");
      const link = join(root, "config.json");
      writeFileSync(target, "{}\n", { mode: 0o600 });
      symlinkSync(target, link);
      let contacted = false;
      expect(() =>
        applyStage6ProtectedEnvironmentConfiguration(
          { configurationPath: link, repository, reviewers },
          {
            runGh: () => {
              contacted = true;
              return { stdout: "{}", stderr: "" };
            },
          },
        ),
      ).toThrow("regular file");
      expect(contacted).toBe(false);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  it("rejects run drift or inadequate source-branch protection before any write", () => {
    const runCalls: Stage6GhCommandInput[] = [];
    expect(() =>
      applyStage6ProtectedEnvironmentConfiguration(
        { configurationPath: "/private/config.json", repository, reviewers },
        {
          readConfiguration: () => ({ config, sha256: `sha256:${"d".repeat(64)}` }),
          runGh: (input) => {
            runCalls.push(input);
            return {
              stdout: JSON.stringify(workflowRunResponse({ head_sha: "f".repeat(40) })),
              stderr: "",
            };
          },
        },
      ),
    ).toThrow("source commit");
    expect(runCalls).toHaveLength(1);

    const branchCalls: Stage6GhCommandInput[] = [];
    expect(() =>
      applyStage6ProtectedEnvironmentConfiguration(
        { configurationPath: "/private/config.json", repository, reviewers },
        {
          readConfiguration: () => ({ config, sha256: `sha256:${"d".repeat(64)}` }),
          runGh: (input) => {
            branchCalls.push(input);
            if (input.args[1]?.includes("/actions/runs/")) {
              return { stdout: JSON.stringify(workflowRunResponse()), stderr: "" };
            }
            return {
              stdout: JSON.stringify({
                ...branchProtectionResponse(),
                enforce_admins: { enabled: false },
              }),
              stderr: "",
            };
          },
        },
      ),
    ).toThrow("must enforce administrators");
    expect(branchCalls).toHaveLength(2);
  });

  it("fails closed when GitHub readback changes a candidate variable", () => {
    expect(() =>
      applyStage6ProtectedEnvironmentConfiguration(
        { configurationPath: "/private/config.json", repository, reviewers },
        {
          readConfiguration: () => ({
            config,
            sha256: `sha256:${"d".repeat(64)}`,
          }),
          runGh: (input) => {
            if (input.args[0] === "api") {
              if (input.args.length === 2) {
                return {
                  stdout: JSON.stringify(apiReadResponse(input.args[1]!)),
                  stderr: "",
                };
              }
              return {
                stdout: JSON.stringify(environmentResponse()),
                stderr: "",
              };
            }
            if (input.args[0] === "variable" && input.args[1] === "list") {
              return {
                stdout: JSON.stringify(
                  variableList().map((variable) =>
                    variable.name === "SYNARA_STAGE6_CANDIDATE_ID"
                      ? { name: variable.name, value: "different-candidate" }
                      : variable,
                  ),
                ),
                stderr: "",
              };
            }
            if (input.args[0] === "secret" && input.args[1] === "list") {
              return {
                stdout: JSON.stringify([{ name: "SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64" }]),
                stderr: "",
              };
            }
            return { stdout: "", stderr: "" };
          },
        },
      ),
    ).toThrow("does not match the candidate");
  });
});
