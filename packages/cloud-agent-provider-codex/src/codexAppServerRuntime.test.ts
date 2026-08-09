import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  CODEX_DEFAULT_MODE_DEVELOPER_INSTRUCTIONS,
  CODEX_PLAN_MODE_DEVELOPER_INSTRUCTIONS,
} from "./codexCollaborationMode";
import {
  CODEX_DISABLED_RUNTIME_FEATURES,
  CODEX_HOSTED_TOOL_ISOLATION_CONFIG,
} from "./codexRuntimeIsolation";
import { PROVIDER_CONTENT_TRUST_POLICY_MARKER } from "./providerContentTrustPolicy";
import { describe, expect, it } from "vitest";

import {
  codexThreadOpenPermissions,
  isManagedCodexToolPolicyHookAttested,
  MANAGED_CODEX_APP_SERVER_ARGUMENTS,
  managedCodexAppServerArguments,
} from "./codexAppServerRuntime";
import { CODEX_TOOL_POLICY_HOOK_ARGUMENT } from "./codexPostToolUseProvenance";
import type { RunnerMessage } from "@synara/cloud-agent-provider-api/internal";
import { PROVIDER_OUTER_SANDBOX_PROFILE_ENV } from "@synara/cloud-agent-provider-api/internal";
import { startCodexProviderRun as startProviderHostRun } from "./index";

process.env[PROVIDER_OUTER_SANDBOX_PROFILE_ENV] = "single-tenant-trusted-v1";

const CONTROLLED_PROVIDER_PROXY = "http://proxy.example.test:8080";
const TEST_CODEX_TOOL_POLICY_HOOK_COMMAND = `'/usr/local/bin/node' '/opt/synara/provider-host/index.mjs' ${CODEX_TOOL_POLICY_HOOK_ARGUMENT}`;

describe("Codex app-server runtime", () => {
  it("disables repository or user configured MCP servers in managed runs", () => {
    expect(MANAGED_CODEX_APP_SERVER_ARGUMENTS.slice(0, 4)).toEqual([
      "app-server",
      "--strict-config",
      "--config",
      "mcp_servers={}",
    ]);
    for (const config of CODEX_HOSTED_TOOL_ISOLATION_CONFIG) {
      expect(MANAGED_CODEX_APP_SERVER_ARGUMENTS).toContain(config);
    }
    for (const feature of CODEX_DISABLED_RUNTIME_FEATURES) {
      expect(MANAGED_CODEX_APP_SERVER_ARGUMENTS).toContain(`features.${feature}=false`);
    }
  });

  it("activates only the exact Host-owned tool-policy hook for controlled credentials", () => {
    const hookCommand = TEST_CODEX_TOOL_POLICY_HOOK_COMMAND;
    const args = managedCodexAppServerArguments(hookCommand);
    const hookConfigs = args.filter(
      (argument) =>
        argument.startsWith("hooks.PreToolUse=") || argument.startsWith("hooks.PostToolUse="),
    );

    expect(args.slice(0, 5)).toEqual([
      "--dangerously-bypass-hook-trust",
      "app-server",
      "--strict-config",
      "--config",
      "mcp_servers={}",
    ]);
    expect(hookConfigs).toHaveLength(1);
    expect(hookConfigs[0]).toContain("hooks.PreToolUse=");
    for (const hookConfig of hookConfigs) {
      expect(hookConfig).toContain(JSON.stringify(hookCommand));
      expect(hookConfig).toContain("additionalContextLimit=512");
      expect(hookConfig).not.toContain("tool_response");
    }
    expect(args.lastIndexOf("features.hooks=false")).toBeLessThan(
      args.lastIndexOf("features.hooks=true"),
    );
    for (const feature of CODEX_DISABLED_RUNTIME_FEATURES) {
      if (feature !== "hooks") {
        expect(args).toContain(`features.${feature}=false`);
        expect(args).not.toContain(`features.${feature}=true`);
      }
    }

    const exactHook = () => ({
      eventName: "preToolUse",
      handlerType: "command",
      matcher: ".*",
      command: hookCommand,
      timeoutSec: 5,
      additionalContextLimit: 512,
      source: "sessionFlags",
      trustStatus: "untrusted",
      enabled: true,
      isManaged: false,
    });
    expect(
      isManagedCodexToolPolicyHookAttested(
        { data: [{ hooks: [exactHook()], errors: [] }] },
        hookCommand,
      ),
    ).toBe(true);
    expect(
      isManagedCodexToolPolicyHookAttested(
        {
          data: [
            {
              hooks: [
                exactHook(),
                {
                  ...exactHook(),
                  command: "/workspace/.codex/attacker-hook",
                },
              ],
              errors: [],
            },
          ],
        },
        hookCommand,
      ),
    ).toBe(false);
  });

  it("includes the fixed content-trust policy in every collaboration mode", () => {
    expect(CODEX_DEFAULT_MODE_DEVELOPER_INSTRUCTIONS).toContain(
      PROVIDER_CONTENT_TRUST_POLICY_MARKER,
    );
    expect(CODEX_PLAN_MODE_DEVELOPER_INSTRUCTIONS).toContain(PROVIDER_CONTENT_TRUST_POLICY_MARKER);
  });

  it("uses durable approval without an unavailable nested container sandbox", () => {
    expect(codexThreadOpenPermissions("approval-required", true)).toEqual({
      approvalPolicy: "untrusted",
      approvalsReviewer: "user",
      sandbox: "danger-full-access",
    });
    expect(codexThreadOpenPermissions("full-access", true)).toEqual({
      approvalPolicy: "never",
      approvalsReviewer: "user",
      sandbox: "danger-full-access",
    });
    expect(codexThreadOpenPermissions("full-access", false, true)).toEqual({
      approvalPolicy: "never",
      approvalsReviewer: "user",
      sandbox: "read-only",
    });
  });

  it("isolates controlled Credential authentication from ambient CODEX_HOME", async () => {
    await withFakeCodex("credential-environment", async (directory, _tracePath, environment) => {
      const providerStateDirectory = join(directory, "provider-state");
      const run = startProviderHostRun(
        codexInput(directory, { runtimeOutputDirectory: directory, providerStateDirectory }),
        {
          payload: {
            apiKey: "provider-secret",
            baseUrl: "http://provider-fault.example.test/v1",
          },
        },
        () => {},
        {
          environment,
          codexToolPolicyHookCommand: TEST_CODEX_TOOL_POLICY_HOOK_COMMAND,
        },
      );

      await expect(run.result).resolves.toMatchObject({ output: { text: "credential isolated" } });
      const config = readFileSync(
        join(providerStateDirectory, "codex-home", "config.toml"),
        "utf8",
      );
      expect(config).toContain('model_provider = "synara_controlled"');
      expect(config).toContain('base_url = "http://provider-fault.example.test/v1"');
      expect(config).toContain('env_key = "OPENAI_API_KEY"');
      expect(config).toContain("requires_openai_auth = false");
      expect(config).not.toContain("provider-secret");
    });
  });

  it("keeps the Host tool policy enabled with ambient authentication", async () => {
    await withFakeCodex("ambient-hook", async (directory, _tracePath, environment) => {
      const run = startProviderHostRun(
        codexInput(directory, { runtimeOutputDirectory: directory }),
        null,
        () => {},
        {
          environment,
          codexToolPolicyHookCommand: TEST_CODEX_TOOL_POLICY_HOOK_COMMAND,
        },
      );

      await expect(run.result).resolves.toMatchObject({
        output: { text: "ambient hook isolated" },
      });
    });
  });

  it("stores an oversized native Turn Diff as a Runtime Output ArtifactCandidate", async () => {
    await withFakeCodex("large-diff", async (directory, _tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const run = startProviderHostRun(
        codexInput(directory, { runtimeOutputDirectory: directory }),
        null,
        (message) => messages.push(message),
        { environment },
      );

      await expect(run.result).resolves.toMatchObject({ output: { text: "large diff ready" } });
      const diffArtifact = messages.find(
        (message) => message.type === "artifact" && message.artifact.kind === "diff",
      );
      expect(diffArtifact).toMatchObject({
        type: "artifact",
        artifact: {
          kind: "diff",
          sourceRoot: "runtime-output",
          contentType: "text/x-diff; charset=utf-8",
          fileCount: 1,
          additions: 5_000,
          deletions: 1,
        },
      });
      if (diffArtifact?.type !== "artifact") throw new Error("Expected a Diff ArtifactCandidate.");
      expect(readFileSync(join(directory, diffArtifact.artifact.path), "utf8")).toContain(
        "diff --git a/large.txt b/large.txt",
      );
      expect(JSON.stringify(messages)).not.toContain(directory);
    });
  });

  it("emits one standalone generated-file candidate from a completed native file-change item", async () => {
    await withFakeCodex("generated-file", async (directory, _tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const run = startProviderHostRun(
        codexInput(directory),
        null,
        (message) => messages.push(message),
        { environment },
      );

      await expect(run.result).resolves.toMatchObject({ output: { text: "file generated" } });
      const artifacts = messages.filter((message) => message.type === "artifact");
      const generatedFile = readFileSync(
        join(directory, ".synara-stage3-acceptance", "generated-file.txt"),
        "utf8",
      );
      expect(artifacts, JSON.stringify({ generatedFile, messages }, null, 2)).toEqual([
        {
          type: "artifact",
          artifact: {
            path: join(".synara-stage3-acceptance", "generated-file.txt"),
            kind: "generated_file",
            contentType: "application/octet-stream",
            sourceRoot: "workspace",
          },
        },
      ]);
      expect(JSON.stringify(messages)).not.toContain(directory);
    });
  });

  it("emits safe terminal lifecycle metadata and bounded command output", async () => {
    await withFakeCodex("terminal", async (directory, _tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const run = startProviderHostRun(
        codexInput(directory),
        null,
        (message) => messages.push(message),
        { environment },
      );

      await expect(run.result).resolves.toMatchObject({ output: { text: "terminal complete" } });
      const output = messages.filter(
        (message): message is Extract<RunnerMessage, { type: "event" }> =>
          message.type === "event" && message.eventType === "runtime.command.output",
      );
      expect(output.map((message) => message.payload.text).join("")).toBe("tests passed\n");
      expect(output.map((message) => message.payload.byteOffset)).toEqual([0, 6]);
      expect(output.map((message) => message.payload.byteLength)).toEqual([6, 7]);
      expect(messages).toContainEqual({
        type: "event",
        eventType: "runtime.provider.activity",
        payload: expect.objectContaining({
          provider: "codex",
          itemType: "commandExecution",
          itemId: "command-terminal-1",
          status: "started",
          terminalId: "command-terminal-1",
          terminalEventType: "terminal.started",
          commandSummary: "bun run test",
          cwdLabel: ".",
        }),
      });
      expect(messages).toContainEqual({
        type: "event",
        eventType: "runtime.provider.activity",
        payload: expect.objectContaining({
          status: "completed",
          terminalEventType: "terminal.exited",
          exitCode: 0,
        }),
      });
    });
  });

  it("drains an authoritative command completion delivered after turn completion", async () => {
    await withFakeCodex("terminal-after-turn", async (directory, _tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const run = startProviderHostRun(
        codexInput(directory),
        null,
        (message) => messages.push(message),
        { environment },
      );

      await expect(run.result).resolves.toMatchObject({
        output: { text: "terminal complete" },
      });
      expect(messages).toContainEqual(
        expect.objectContaining({
          type: "event",
          eventType: "runtime.provider.activity",
          payload: expect.objectContaining({
            itemId: "command-terminal-late",
            status: "completed",
            terminalEventType: "terminal.exited",
            exitCode: 0,
          }),
        }),
      );
      expect(messages).not.toContainEqual(
        expect.objectContaining({
          type: "event",
          eventType: "runtime.provider.activity",
          payload: expect.objectContaining({
            itemId: "command-terminal-late",
            status: "failed",
          }),
        }),
      );
      expect(messages).not.toContainEqual(expect.objectContaining({ type: "interaction" }));
      expect(messages).not.toContainEqual(
        expect.objectContaining({
          type: "event",
          eventType: "runtime.usage",
          payload: expect.objectContaining({ inputTokens: 987_654 }),
        }),
      );
      expect(messages).not.toContainEqual(
        expect.objectContaining({
          type: "event",
          eventType: "runtime.provider.warning",
          payload: expect.objectContaining({ message: "late terminal warning" }),
        }),
      );
    });
  });

  it("ignores a duplicate command completion before it can flush late output", async () => {
    await withFakeCodex(
      "terminal-duplicate-completion",
      async (directory, _tracePath, environment) => {
        const messages: RunnerMessage[] = [];
        const run = startProviderHostRun(
          codexInput(directory),
          null,
          (message) => messages.push(message),
          { environment },
        );

        await expect(run.result).resolves.toMatchObject({ output: { text: "command declined" } });
        const terminalActivity = messages.filter(
          (message): message is Extract<RunnerMessage, { type: "event" }> =>
            message.type === "event" &&
            message.eventType === "runtime.provider.activity" &&
            message.payload.itemId === "command-terminal-duplicate",
        );
        expect(terminalActivity.map((message) => message.payload.status)).toEqual([
          "started",
          "declined",
        ]);
        expect(messages).not.toContainEqual(
          expect.objectContaining({
            type: "event",
            eventType: "runtime.command.output",
            payload: expect.objectContaining({ text: "late duplicate output" }),
          }),
        );
      },
    );
  });

  it("fails closed when a completed turn never closes its command item", async () => {
    await withFakeCodex(
      "terminal-missing-completion",
      async (directory, _tracePath, environment) => {
        const messages: RunnerMessage[] = [];
        const run = startProviderHostRun(
          codexInput(directory),
          null,
          (message) => messages.push(message),
          { environment },
        );

        await expect(run.result).rejects.toThrow(
          "Codex app-server completed a Turn with an open command execution.",
        );
        expect(messages).toContainEqual(
          expect.objectContaining({
            type: "event",
            eventType: "runtime.provider.activity",
            payload: expect.objectContaining({
              itemId: "command-terminal-missing",
              status: "failed",
              terminalEventType: "terminal.failed",
            }),
          }),
        );
      },
    );
  }, 15_000);

  it("delivers a native approval response and returns streamed output", async () => {
    await withFakeCodex("approval", async (directory, _tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const interaction = waitForInteraction(
        messages,
        (message) => message.interactionType === "approval",
      );
      const run = startProviderHostRun(
        codexInput(directory),
        null,
        (message) => messages.push(message),
        { environment },
      );

      const request = await interaction;
      expect(request.payload).toMatchObject({
        requestId: "codex:approval-rpc",
        requestKind: "command",
        command: "git push origin main",
        sensitiveAction: {
          categories: ["protected-branch-publish"],
          requiresFreshApproval: true,
          allowSessionApproval: false,
        },
      });
      await run.resolveApproval?.({
        requestId: request.payload.requestId,
        resolution: { decision: "accept" },
      });

      await expect(run.result).resolves.toMatchObject({
        output: { provider: "codex", text: "approved" },
        providerResumeCursor: "thread-new",
      });
      expect(messages).toContainEqual({
        type: "event",
        eventType: "runtime.usage",
        payload: {
          provider: "codex",
          totalTokens: 8,
          inputTokens: 5,
          cachedInputTokens: 1,
          outputTokens: 3,
          reasoningOutputTokens: 0,
          compactsAutomatically: true,
        },
      });
    });
  });

  it("correlates file-change item paths into the native approval assessment", async () => {
    await withFakeCodex("file-change-approval", async (directory, _tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const interaction = waitForInteraction(
        messages,
        (message) => message.interactionType === "approval",
      );
      const run = startProviderHostRun(
        codexInput(directory),
        null,
        (message) => messages.push(message),
        { environment },
      );

      const request = await interaction;
      expect(request.payload).toMatchObject({
        requestId: "codex:file-approval-rpc",
        requestKind: "file-change",
        itemId: "file-change-1",
        sensitiveAction: {
          categories: ["dependency-change"],
          requiresFreshApproval: true,
          allowSessionApproval: false,
        },
      });
      await run.resolveApproval?.({
        requestId: request.payload.requestId,
        resolution: { decision: "decline" },
      });

      await expect(run.result).resolves.toMatchObject({
        output: { text: "patch declined" },
      });
      expect(messages).toContainEqual(
        expect.objectContaining({
          type: "event",
          eventType: "runtime.provider.activity",
          payload: expect.objectContaining({
            itemType: "fileChange",
            itemId: "file-change-1",
            status: "declined",
          }),
        }),
      );
    });
  });

  it("fails closed when file-change approval has no preceding item classification", async () => {
    await withFakeCodex("file-change-missing-item", async (directory, _tracePath, environment) => {
      const run = startProviderHostRun(codexInput(directory), null, () => {}, {
        environment,
      });

      await expect(run.result).rejects.toThrow(
        "Codex file-change approval arrived without a preceding Host-classified item/started notification.",
      );
    });
  });

  it("answers native structured user input", async () => {
    await withFakeCodex("user-input", async (directory, _tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const interaction = waitForInteraction(
        messages,
        (message) => message.interactionType === "user-input",
      );
      const run = startProviderHostRun(
        codexInput(directory, { interactionMode: "plan" }),
        null,
        (message) => messages.push(message),
        { environment },
      );

      const request = await interaction;
      expect(request.payload).toMatchObject({
        requestId: "codex:user-input-rpc",
        questions: [
          {
            id: "environment",
            header: "Environment",
            question: "Where should this run?",
          },
        ],
      });
      await run.resolveUserInput?.({
        requestId: request.payload.requestId,
        resolution: { answers: { environment: "staging" } },
      });

      await expect(run.result).resolves.toMatchObject({
        output: { text: "staging" },
      });
    });
  });

  it.each([
    {
      scenario: "approval",
      generation: 7,
      interactionMode: "default",
      interactionType: "approval",
      expectedRequestId: "codex:generation-7:approval-rpc",
    },
    {
      scenario: "user-input",
      generation: 8,
      interactionMode: "plan",
      interactionType: "user-input",
      expectedRequestId: "codex:generation-8:user-input-rpc",
    },
  ] as const)(
    "scopes native $interactionType request IDs to Execution Generation $generation",
    async ({ scenario, generation, interactionMode, interactionType, expectedRequestId }) => {
      await withFakeCodex(scenario, async (directory, _tracePath, environment) => {
        const messages: RunnerMessage[] = [];
        const interaction = waitForInteraction(
          messages,
          (message) => message.interactionType === interactionType,
        );
        const run = startProviderHostRun(
          codexInput(directory, { generation, interactionMode }),
          null,
          (message) => messages.push(message),
          { environment },
        );

        const request = await interaction;
        expect(request.payload.requestId).toBe(expectedRequestId);
        if (interactionType === "approval") {
          await run.resolveApproval?.({
            requestId: request.payload.requestId,
            resolution: { decision: "accept" },
          });
        } else {
          await run.resolveUserInput?.({
            requestId: request.payload.requestId,
            resolution: { answers: { environment: "staging" } },
          });
        }
        await expect(run.result).resolves.toMatchObject({ type: "result" });
      });
    },
  );

  it("bounds long native request IDs while preserving the Codex Generation scope", async () => {
    const requestIds: string[] = [];
    for (const generation of [10, 11]) {
      await withFakeCodex("long-approval", async (directory, _tracePath, environment) => {
        const messages: RunnerMessage[] = [];
        const interaction = waitForInteraction(
          messages,
          (message) => message.interactionType === "approval",
        );
        const run = startProviderHostRun(
          codexInput(directory, { generation }),
          null,
          (message) => messages.push(message),
          { environment },
        );

        const request = await interaction;
        const requestId = String(request.payload.requestId);
        requestIds.push(requestId);
        expect(Buffer.byteLength(requestId)).toBeLessThanOrEqual(200);
        expect(requestId).toMatch(new RegExp(`^codex:generation-${generation}:`));
        await run.resolveApproval?.({
          requestId,
          resolution: { decision: "accept" },
        });
        await expect(run.result).resolves.toMatchObject({ output: { text: "approved" } });
      });
    }

    expect(requestIds[1]).not.toBe(requestIds[0]);
  });

  it("uses thread/resume even when authoritative history is available", async () => {
    await withFakeCodex("resume", async (directory, _tracePath, environment) => {
      const run = startProviderHostRun(
        {
          ...codexInput(directory),
          providerResumeCursor: "thread-resume",
          workload: {
            provider: "codex",
            inputText: "continue",
            conversationHistory: [
              { role: "user", text: "first" },
              { role: "assistant", text: "response" },
            ],
          },
        },
        null,
        () => {},
        { environment },
      );

      await expect(run.result).resolves.toMatchObject({
        output: { text: "resumed" },
        providerResumeCursor: "thread-resume",
      });
    });
  });

  it("sends recovery metadata in the resumed turn when native resume succeeds", async () => {
    await withFakeCodex(
      "resume-with-recovery-metadata",
      async (directory, _tracePath, environment) => {
        const run = startProviderHostRun(
          {
            ...codexInput(directory),
            providerResumeCursor: "thread-resume",
            workload: {
              provider: "codex",
              inputText: "continue from the suspended approval",
              resumeSnapshot: {
                version: 1,
                sessionId: "session-1",
                turnId: "turn-2",
                provider: "codex",
                messages: [
                  { role: "user", text: "prior native question" },
                  { role: "assistant", text: "prior native answer" },
                ],
                resumeRecordedInteractions: [
                  {
                    kind: "approval",
                    requestId: "approval-suspended",
                    resolutionKind: "approved",
                    resolution: { decision: "accept" },
                  },
                ],
                workspace: {
                  workspaceId: "workspace-1",
                  checkpoint: { checkpointId: "checkpoint-1", strategy: "git-reference" },
                },
              },
            },
            memoryDocuments: [
              {
                scope: "session",
                scopeId: "session-1",
                memoryKey: "instructions",
                revisionId: "revision-1",
                artifactId: "artifact-1",
                sha256: "c".repeat(64),
                contentType: "text/plain",
                content: "Remember the preferred deployment environment.",
              },
            ],
          },
          null,
          () => {},
          { environment },
        );

        await expect(run.result).resolves.toMatchObject({
          output: { text: "resumed with metadata" },
          providerResumeCursor: "thread-resume",
        });
      },
    );
  });

  it("waits for a real contextCompaction terminal after thread/compact/start acknowledgement", async () => {
    await withFakeCodex("compact", async (directory, tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const run = startProviderHostRun(
        { ...codexInput(directory), providerResumeCursor: "thread-resume" },
        null,
        (message) => messages.push(message),
        {
          environment,
          operation: { commandType: "CompactSession", payload: {} },
        },
      );

      const result = await run.result;
      expect(result).toMatchObject({
        output: {
          provider: "codex",
          operation: "compact",
          supportMode: "native",
          boundary: {
            kind: "context_compaction",
            terminalKind: "contextCompaction",
            summaryAvailable: false,
            providerItemId: "compact-item-1",
          },
        },
        providerResumeCursor: "thread-resume",
      });
      await new Promise((resolve) => setTimeout(resolve, 10));
      expect(messages).toContainEqual(
        expect.objectContaining({
          type: "event",
          eventType: "runtime.provider.activity",
          payload: expect.objectContaining({
            itemType: "contextCompaction",
            status: "completed",
          }),
        }),
      );
      expect(messages).not.toContainEqual(
        expect.objectContaining({
          type: "event",
          eventType: "runtime.usage",
          payload: expect.objectContaining({ inputTokens: 987_654 }),
        }),
      );
      expect(messages).not.toContainEqual(
        expect.objectContaining({
          type: "event",
          eventType: "runtime.provider.warning",
          payload: expect.objectContaining({ message: "late compact warning" }),
        }),
      );
      expect(messages).not.toContainEqual(expect.objectContaining({ type: "interaction" }));
      expect(readFileSync(tracePath, "utf8")).toBe(
        "initialize\ninitialized\nthread/resume\nthread/compact/start\n",
      );
    });
  });

  it("rebuilds a missing native Compact thread from authoritative history before compacting", async () => {
    await withFakeCodex("compact-rebuild", async (directory, tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const input = codexInput(directory);
      const run = startProviderHostRun(
        {
          ...input,
          providerResumeCursor: "thread-missing",
          workload: {
            ...input.workload,
            resumeSnapshot: {
              version: 1,
              sessionId: "session-1",
              turnId: "turn-2",
              provider: "codex",
              messages: [
                { role: "user", text: "first" },
                { role: "assistant", text: "response" },
              ],
              toolResults: [{ summary: "Focused tests passed" }],
              sourceSequenceRange: { from: 1, through: 4 },
              authoritativeHistorySequence: 23,
            },
          },
        },
        null,
        (message) => messages.push(message),
        {
          environment,
          operation: { commandType: "CompactSession", payload: {} },
        },
      );

      await expect(run.result).resolves.toMatchObject({
        output: {
          provider: "codex",
          operation: "compact",
          supportMode: "native",
          boundary: {
            kind: "context_compaction",
            terminalKind: "contextCompaction",
            providerItemId: "compact-item-1",
          },
        },
        providerResumeCursor: "thread-rebuilt",
      });
      const warning = messages.find(
        (message) => message.type === "event" && message.eventType === "runtime.provider.warning",
      );
      expect(warning).toEqual({
        type: "event",
        eventType: "runtime.provider.warning",
        payload: {
          provider: "codex",
          message:
            "Native Provider resume failed before turn activity; authoritative-history fallback selected.",
          kind: "session_resume",
          attemptedStrategy: "native-cursor",
          selectedStrategy: "authoritative-history",
          outcome: "fallback_selected",
          reasonCode: "session_resume_invalid",
          fallbackSafety: "before_turn_activity",
          authoritativeHistorySequence: 23,
        },
      });
      expect(JSON.stringify(warning)).not.toContain("thread-missing");
      expect(JSON.stringify(warning)).not.toContain("Focused tests passed");
      expect(readFileSync(tracePath, "utf8")).toBe(
        "initialize\ninitialized\nthread/resume\nthread/resume\nthread/compact/start\n",
      );
    });
  });

  it("does not rebuild Compact from history after a native resume authentication failure", async () => {
    await withFakeCodex("compact-auth-failure", async (directory, tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const input = codexInput(directory);
      const run = startProviderHostRun(
        {
          ...input,
          providerResumeCursor: "thread-private",
          workload: {
            ...input.workload,
            resumeSnapshot: {
              version: 1,
              sessionId: "session-1",
              turnId: "turn-2",
              provider: "codex",
              messages: [{ role: "assistant", text: "authoritative response" }],
              authoritativeHistorySequence: 24,
            },
          },
        },
        null,
        (message) => messages.push(message),
        {
          environment,
          operation: { commandType: "CompactSession", payload: {} },
        },
      );

      await expect(run.result).rejects.toThrow("Unauthorized");
      expect(messages).not.toContainEqual(
        expect.objectContaining({ eventType: "runtime.provider.warning" }),
      );
      expect(readFileSync(tracePath, "utf8")).toBe("initialize\ninitialized\nthread/resume\n");
    });
  });

  it("runs native inline review and waits for the matching review Turn terminal", async () => {
    await withFakeCodex("review", async (directory, tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const run = startProviderHostRun(
        { ...codexInput(directory), providerResumeCursor: "thread-resume" },
        null,
        (message) => messages.push(message),
        {
          environment,
          operation: {
            commandType: "StartReview",
            payload: { target: { type: "baseBranch", branch: "main" } },
          },
        },
      );

      await expect(run.result).resolves.toMatchObject({
        output: {
          provider: "codex",
          operation: "review",
          supportMode: "native",
          providerTurnId: "turn-review-1",
          text: "Working tree is clean.",
        },
        providerResumeCursor: "thread-resume",
      });
      expect(messages).toContainEqual(
        expect.objectContaining({
          type: "event",
          eventType: "runtime.provider.activity",
          payload: expect.objectContaining({
            itemType: "enteredReviewMode",
            status: "completed",
          }),
        }),
      );
      expect(messages).toContainEqual(
        expect.objectContaining({
          type: "event",
          eventType: "runtime.provider.activity",
          payload: expect.objectContaining({
            itemType: "exitedReviewMode",
            status: "completed",
          }),
        }),
      );
      expect(readFileSync(tracePath, "utf8")).toBe(
        "initialize\ninitialized\nthread/resume\nreview/start\n",
      );
    });
  });

  it("starts a fresh Codex Thread for review when no native cursor exists", async () => {
    await withFakeCodex("review-fresh", async (directory, tracePath, environment) => {
      const run = startProviderHostRun(codexInput(directory), null, () => {}, {
        environment,
        operation: {
          commandType: "StartReview",
          payload: { target: { type: "baseBranch", branch: "main" } },
        },
      });

      await expect(run.result).resolves.toMatchObject({
        output: {
          operation: "review",
          supportMode: "native",
          providerTurnId: "turn-review-1",
        },
        providerResumeCursor: "thread-new",
      });
      expect(readFileSync(tracePath, "utf8")).toBe(
        "initialize\ninitialized\nthread/start\nreview/start\n",
      );
    });
  });

  it("refuses native compaction before a Provider Cursor exists", async () => {
    await withFakeCodex("compact", async (directory, tracePath, environment) => {
      const run = startProviderHostRun(codexInput(directory), null, () => {}, {
        environment,
        operation: { commandType: "CompactSession", payload: {} },
      });

      await expect(run.result).rejects.toThrow(
        "Codex app-server native Session operation requires a Provider Cursor.",
      );
      const trace = readFileSync(tracePath, "utf8");
      expect(trace).toContain("initialize\n");
      expect(trace).not.toContain("thread/start\n");
      expect(trace).not.toContain("thread/compact/start\n");
    });
  });

  it("falls back to ResumeSnapshot reconstruction when native thread resume is invalid", async () => {
    await withFakeCodex("resume-rebuild", async (directory, _tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const run = startProviderHostRun(
        {
          ...codexInput(directory),
          providerResumeCursor: "thread-missing",
          workload: {
            provider: "codex",
            inputText: "continue",
            resumeSnapshot: {
              version: 1,
              sessionId: "session-1",
              turnId: "turn-2",
              provider: "codex",
              messages: [
                { role: "user", text: "first" },
                { role: "assistant", text: "response" },
              ],
              toolResults: [{ summary: "Focused tests passed" }],
              sourceSequenceRange: { from: 1, through: 4 },
              authoritativeHistorySequence: 19,
            },
          },
        },
        null,
        (message) => messages.push(message),
        { environment },
      );

      await expect(run.result).resolves.toMatchObject({
        output: { text: "rebuilt" },
        providerResumeCursor: "thread-new",
      });
      expect(messages).toContainEqual({
        type: "event",
        eventType: "runtime.provider.warning",
        payload: {
          provider: "codex",
          message:
            "Native Provider resume failed before turn activity; authoritative-history fallback selected.",
          kind: "session_resume",
          attemptedStrategy: "native-cursor",
          selectedStrategy: "authoritative-history",
          outcome: "fallback_selected",
          reasonCode: "session_resume_invalid",
          fallbackSafety: "before_turn_activity",
          authoritativeHistorySequence: 19,
        },
      });
      expect(JSON.stringify(messages)).not.toContain("native-resume-secret");
      expect(JSON.stringify(messages)).not.toContain("thread-missing");
    });
  });

  it("does not rebuild from history for a native resume authentication failure", async () => {
    await withFakeCodex("resume-auth-failure", async (directory, tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const run = startProviderHostRun(
        {
          ...codexInput(directory),
          providerResumeCursor: "thread-private",
          workload: {
            provider: "codex",
            inputText: "continue",
            resumeSnapshot: {
              version: 1,
              sessionId: "session-1",
              turnId: "turn-2",
              provider: "codex",
              messages: [{ role: "assistant", text: "authoritative response" }],
              authoritativeHistorySequence: 20,
            },
          },
        },
        null,
        (message) => messages.push(message),
        { environment },
      );

      await expect(run.result).rejects.toThrow("Unauthorized");
      expect(messages).not.toContainEqual(
        expect.objectContaining({ eventType: "runtime.provider.warning" }),
      );
      expect(readFileSync(tracePath, "utf8")).not.toContain("thread/start");
    });
  });

  it("preserves a credential-free controlled proxy in Provider output", async () => {
    await withFakeCodex("proxy-output", async (directory, _tracePath, environment) => {
      const run = startProviderHostRun(codexInput(directory), null, () => {}, { environment });
      const result = await run.result;

      expect(result).toMatchObject({
        output: { provider: "codex", text: CONTROLLED_PROVIDER_PROXY },
      });
      expect(JSON.stringify(result)).toContain(CONTROLLED_PROVIDER_PROXY);
    });
  });

  it("sends native turn/interrupt before terminating the app-server", async () => {
    await withFakeCodex("interrupt", async (directory, tracePath, environment) => {
      let started!: () => void;
      const startedPromise = new Promise<void>((resolve) => {
        started = resolve;
      });
      const run = startProviderHostRun(
        codexInput(directory),
        null,
        (message) => {
          if (message.type === "event" && message.eventType === "runtime.output.delta") started();
        },
        { environment },
      );

      await startedPromise;
      run.interrupt();
      await expect(run.result).rejects.toThrow("interrupted");
      expect(readFileSync(tracePath, "utf8")).toContain("turn/interrupt");
    });
  });

  it("steers the active native Turn without creating a second Turn", async () => {
    await withFakeCodex("steer", async (directory, tracePath, environment) => {
      let started!: () => void;
      const startedPromise = new Promise<void>((resolve) => {
        started = resolve;
      });
      const run = startProviderHostRun(
        codexInput(directory),
        null,
        (message) => {
          if (message.type === "event" && message.eventType === "runtime.output.delta") started();
        },
        { environment },
      );

      await startedPromise;
      await run.steer?.({ inputText: "focus on the failing test" });
      await expect(run.result).resolves.toMatchObject({
        output: { text: "workingsteered" },
        providerResumeCursor: "thread-new",
      });
      expect(readFileSync(tracePath, "utf8")).toContain("turn/steer");
    });
  });
});

function codexInput(
  directory: string,
  modes: {
    interactionMode?: "default" | "plan";
    generation?: number;
    runtimeOutputDirectory?: string;
    providerStateDirectory?: string;
  } = {},
) {
  return {
    execution: {
      id: "execution-codex-app-server",
      ...(modes.generation === undefined ? {} : { generation: modes.generation }),
    },
    workload: {
      provider: "codex",
      model: "gpt-test",
      inputText: "do work",
      runtimeMode: "approval-required" as const,
      interactionMode: modes.interactionMode ?? "default",
    },
    workspaceDirectory: directory,
    ...(modes.runtimeOutputDirectory
      ? { runtimeOutputDirectory: modes.runtimeOutputDirectory }
      : {}),
    ...(modes.providerStateDirectory
      ? { providerStateDirectory: modes.providerStateDirectory }
      : {}),
  };
}

function waitForInteraction(
  messages: RunnerMessage[],
  predicate: (message: Extract<RunnerMessage, { type: "interaction" }>) => boolean,
): Promise<Extract<RunnerMessage, { type: "interaction" }>> {
  return new Promise((resolve) => {
    const timer = setInterval(() => {
      const message = messages.find(
        (item): item is Extract<RunnerMessage, { type: "interaction" }> =>
          item.type === "interaction" && predicate(item),
      );
      if (!message) return;
      clearInterval(timer);
      resolve(message);
    }, 1);
  });
}

async function withFakeCodex(
  scenario:
    | "approval"
    | "long-approval"
    | "user-input"
    | "resume"
    | "resume-with-recovery-metadata"
    | "resume-rebuild"
    | "resume-auth-failure"
    | "interrupt"
    | "steer"
    | "proxy-output"
    | "terminal"
    | "terminal-after-turn"
    | "terminal-duplicate-completion"
    | "terminal-missing-completion"
    | "generated-file"
    | "large-diff"
    | "compact"
    | "compact-rebuild"
    | "compact-auth-failure"
    | "review"
    | "review-fresh"
    | "file-change-approval"
    | "file-change-missing-item"
    | "ambient-hook"
    | "credential-environment",
  run: (directory: string, tracePath: string, environment: NodeJS.ProcessEnv) => Promise<void>,
): Promise<void> {
  const directory = mkdtempSync(join(tmpdir(), "synara-codex-app-server-"));
  const executable = join(directory, "codex");
  const tracePath = join(directory, "trace.log");
  writeFileSync(executable, fakeCodexSource(scenario, tracePath, directory), "utf8");
  chmodSync(executable, 0o700);
  const environment: NodeJS.ProcessEnv = {
    ...process.env,
    PATH: `${directory}:${process.env.PATH ?? ""}`,
    HOME: directory,
    TMPDIR: directory,
    LANG: "C.UTF-8",
    TERM: "xterm-256color",
    SECRET: "ordinary-secret",
    HOST_SECRET: "host-secret",
    SYNARA_AUTH_TOKEN: "auth-secret",
    SYNARA_WORKER_REGISTRATION_TOKEN: "worker-secret",
    SYNARA_LEASE_TOKEN: "lease-secret",
    SYNARA_CONTROL_PLANE_URL: "https://control.example.test",
    OPENAI_API_KEY: "ambient-openai-secret",
    ANTHROPIC_API_KEY: "ambient-anthropic-secret",
    AWS_ACCESS_KEY_ID: "aws-key",
    AWS_SECRET_ACCESS_KEY: "aws-secret",
    GITHUB_TOKEN: "github-secret",
    GH_TOKEN: "gh-secret",
    DATABASE_URL: "postgres://user:secret@db/synara",
    PGPASSWORD: "postgres-secret",
    MINIO_ROOT_PASSWORD: "minio-secret",
    GOOGLE_APPLICATION_CREDENTIALS: "/host/gcp-credential.json",
    AZURE_CLIENT_SECRET: "azure-secret",
    HTTP_PROXY: "http://ambient-user:ambient-secret@proxy.example.test",
    SYNARA_PROVIDER_HTTP_PROXY: CONTROLLED_PROVIDER_PROXY,
    SSH_AUTH_SOCK: "/host/agent.sock",
    NODE_OPTIONS: "--require=/host/inject-secrets.js",
  };
  try {
    await run(directory, tracePath, environment);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

function fakeCodexSource(
  scenario:
    | "approval"
    | "long-approval"
    | "user-input"
    | "resume"
    | "resume-with-recovery-metadata"
    | "resume-rebuild"
    | "resume-auth-failure"
    | "interrupt"
    | "steer"
    | "proxy-output"
    | "terminal"
    | "terminal-after-turn"
    | "terminal-duplicate-completion"
    | "terminal-missing-completion"
    | "generated-file"
    | "large-diff"
    | "compact"
    | "compact-rebuild"
    | "compact-auth-failure"
    | "review"
    | "review-fresh"
    | "file-change-approval"
    | "file-change-missing-item"
    | "ambient-hook"
    | "credential-environment",
  tracePath: string,
  directory: string,
): string {
  return `#!/usr/bin/env node
const fs = require("node:fs");
const readline = require("node:readline");
const scenario = ${JSON.stringify(scenario)};
const trace = ${JSON.stringify(tracePath)};
const requiredEnvironment = ${JSON.stringify({
    HOME: directory,
    TMPDIR: directory,
    LANG: "C.UTF-8",
    TERM: "xterm-256color",
    HTTP_PROXY: CONTROLLED_PROVIDER_PROXY,
    ...(scenario === "credential-environment"
      ? {
          OPENAI_API_KEY: "provider-secret",
          OPENAI_BASE_URL: "http://provider-fault.example.test/v1",
          CODEX_HOME: join(directory, "provider-state", "codex-home"),
        }
      : {}),
  })};
for (const [name, value] of Object.entries(requiredEnvironment)) {
  if (process.env[name] !== value) {
    process.stderr.write("required Provider environment mismatch: " + name + "\\n");
    process.exit(90);
  }
}
if (!process.env.PATH) process.exit(91);
for (const name of ${JSON.stringify([
    "SECRET",
    "HOST_SECRET",
    "SYNARA_AUTH_TOKEN",
    "SYNARA_WORKER_REGISTRATION_TOKEN",
    "SYNARA_LEASE_TOKEN",
    "SYNARA_CONTROL_PLANE_URL",
    "OPENAI_API_KEY",
    "OPENAI_BASE_URL",
    "CODEX_HOME",
    "ANTHROPIC_API_KEY",
    "AWS_ACCESS_KEY_ID",
    "AWS_SECRET_ACCESS_KEY",
    "GITHUB_TOKEN",
    "GH_TOKEN",
    "DATABASE_URL",
    "PGPASSWORD",
    "MINIO_ROOT_PASSWORD",
    "GOOGLE_APPLICATION_CREDENTIALS",
    "AZURE_CLIENT_SECRET",
    "SYNARA_PROVIDER_HTTP_PROXY",
    "SSH_AUTH_SOCK",
    "NODE_OPTIONS",
  ])}) {
  if (scenario === "credential-environment" && ["OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME"].includes(name)) continue;
  if (process.env[name] !== undefined) {
    process.stderr.write("ambient secret leaked to Codex child: " + name + "\\n");
    process.exit(92);
  }
}
const appServerArguments = process.argv.slice(2);
const preToolUseHookConfigArgument = appServerArguments.find((value) => value.startsWith("hooks.PreToolUse="));
const postToolUseHookConfigArgument = appServerArguments.find((value) => value.startsWith("hooks.PostToolUse="));
const hookCommandStart = preToolUseHookConfigArgument?.indexOf("command=") ?? -1;
const hookCommandEnd = preToolUseHookConfigArgument?.indexOf(",timeout=", hookCommandStart) ?? -1;
const configuredHookCommand = hookCommandStart >= 0 && hookCommandEnd > hookCommandStart
  ? JSON.parse(preToolUseHookConfigArgument.slice(hookCommandStart + "command=".length, hookCommandEnd))
  : undefined;
const runtimeIsolationFeatures = Object.fromEntries(
  ${JSON.stringify(CODEX_DISABLED_RUNTIME_FEATURES)}.map((feature) => [feature, feature === "hooks"]),
);
if (scenario === "credential-environment" || scenario === "ambient-hook") {
  if (appServerArguments[0] !== "--dangerously-bypass-hook-trust" || appServerArguments[1] !== "app-server") process.exit(93);
  if (!configuredHookCommand?.includes(${JSON.stringify(CODEX_TOOL_POLICY_HOOK_ARGUMENT)})) process.exit(94);
  if (postToolUseHookConfigArgument !== undefined) process.exit(97);
} else if (appServerArguments.includes("--dangerously-bypass-hook-trust") || preToolUseHookConfigArgument !== undefined || postToolUseHookConfigArgument !== undefined) {
  process.exit(95);
}
const send = (message) => process.stdout.write(JSON.stringify(message) + "\\n");
const longApprovalId = "approval-" + "x".repeat(400);
let resumeAttempt = 0;
const complete = (text) => {
  send({ method: "item/agentMessage/delta", params: { threadId: "thread-new", turnId: "turn-1", itemId: "agent-1", delta: text } });
  send({ method: "turn/completed", params: { threadId: "thread-new", turn: { id: "turn-1", items: [], status: "completed", error: null } } });
};
readline.createInterface({ input: process.stdin }).on("line", (line) => {
  const message = JSON.parse(line);
  if (trace && message.method) fs.appendFileSync(trace, message.method + "\\n");
  if (message.method === "initialize") {
    send({ id: message.id, result: { userAgent: "fake" } });
  } else if (message.method === "initialized") {
    return;
  } else if (message.method === "hooks/list") {
    if ((scenario !== "credential-environment" && scenario !== "ambient-hook") || !configuredHookCommand) process.exit(96);
    const hook = (eventName) => ({ eventName, handlerType: "command", matcher: ".*", command: configuredHookCommand, timeoutSec: 5, additionalContextLimit: 512, source: "sessionFlags", trustStatus: "untrusted", enabled: true, isManaged: false });
    send({ id: message.id, result: { data: [{ cwd: process.cwd(), hooks: [hook("preToolUse")], warnings: [], errors: [] }] } });
  } else if (message.method === "config/read") {
    if ((scenario !== "credential-environment" && scenario !== "ambient-hook") || !configuredHookCommand) process.exit(99);
    send({ id: message.id, result: { config: { web_search: "disabled", features: runtimeIsolationFeatures, mcp_servers: {} } } });
  } else if (message.method === "thread/resume") {
    if ((scenario === "credential-environment" || scenario === "ambient-hook") && message.params?.config?.bypass_hook_trust !== true) process.exit(98);
    if (scenario === "compact-rebuild") {
      resumeAttempt += 1;
      if (resumeAttempt === 1) {
        if (message.params?.threadId !== "thread-missing" || message.params?.history !== undefined) process.exit(10);
        send({ id: message.id, error: { code: -2, message: "no rollout found for thread id thread-missing" } });
      } else if (resumeAttempt === 2) {
        const history = message.params?.history;
        const prompt = history?.[0]?.content?.[0]?.text ?? "";
        if (message.params?.threadId !== "thread-missing" || history?.length !== 1 || history?.[0]?.type !== "message" || history?.[0]?.role !== "user" || history?.[0]?.content?.[0]?.type !== "input_text" || !prompt.includes("<synara_resume_snapshot_json>") || !prompt.includes("Focused tests passed")) process.exit(11);
        send({ id: message.id, result: { thread: { id: "thread-rebuilt" }, model: "gpt-test" } });
      } else process.exit(12);
    } else if (scenario === "resume-rebuild") send({ id: message.id, error: { code: -2, message: "missing thread: native-resume-secret" } });
    else if (scenario === "resume-auth-failure" || scenario === "compact-auth-failure") send({ id: message.id, error: { code: 401, message: "Unauthorized: invalid API key" } });
    else send({ id: message.id, result: { thread: { id: message.params.threadId }, model: "gpt-test" } });
  } else if (message.method === "thread/start") {
    if ((scenario === "credential-environment" || scenario === "ambient-hook") && message.params?.config?.bypass_hook_trust !== true) process.exit(98);
    if (scenario === "resume" || scenario === "resume-with-recovery-metadata" || scenario === "resume-auth-failure" || scenario === "compact-rebuild" || scenario === "compact-auth-failure") send({ id: message.id, error: { code: -1, message: "unexpected thread/start" } });
    else send({ id: message.id, result: { thread: { id: "thread-new" }, model: "gpt-test" } });
  } else if (message.method === "thread/compact/start") {
    const compactThreadId = scenario === "compact-rebuild" ? "thread-rebuilt" : "thread-resume";
    if ((scenario !== "compact" && scenario !== "compact-rebuild") || message.params?.threadId !== compactThreadId) process.exit(4);
    send({ method: "turn/completed", params: { threadId: compactThreadId, turn: { id: "turn-compact-1", items: [], status: "completed", error: null } } });
    setTimeout(() => {
      send({ method: "item/completed", params: { threadId: compactThreadId, turnId: "turn-compact-1", item: { id: "compact-item-1", type: "contextCompaction" } } });
      send({ id: "late-compact-approval", method: "item/commandExecution/requestApproval", params: { command: "late compact command" } });
      send({ method: "thread/tokenUsage/updated", params: { threadId: compactThreadId, tokenUsage: { last: { inputTokens: 987654 } } } });
      send({ method: "warning", params: { message: "late compact warning" } });
      send({ id: message.id, result: {} });
    }, 15);
  } else if (message.method === "review/start") {
    const expectedReviewThread = scenario === "review-fresh" ? "thread-new" : "thread-resume";
    if ((scenario !== "review" && scenario !== "review-fresh") || message.params?.threadId !== expectedReviewThread || message.params?.delivery !== "inline" || message.params?.target?.type !== "baseBranch" || message.params?.target?.branch !== "main") process.exit(5);
    send({ id: message.id, result: { turn: { id: "turn-review-1", items: [], status: "inProgress", error: null }, reviewThreadId: "thread-resume" } });
    send({ method: "item/completed", params: { threadId: expectedReviewThread, turnId: "turn-review-1", item: { id: "review-enter-1", type: "enteredReviewMode", review: "" } } });
    send({ method: "item/completed", params: { threadId: expectedReviewThread, turnId: "turn-review-1", item: { id: "review-exit-1", type: "exitedReviewMode", review: "Working tree is clean." } } });
    send({ method: "turn/completed", params: { threadId: expectedReviewThread, turn: { id: "turn-review-1", items: [], status: "completed", error: null } } });
  } else if (message.method === "turn/start") {
    const expectedCollaborationMode = scenario === "user-input" ? "plan" : "default";
    const expectedDeveloperInstructions = scenario === "user-input"
      ? ${JSON.stringify(CODEX_PLAN_MODE_DEVELOPER_INSTRUCTIONS)}
      : ${JSON.stringify(CODEX_DEFAULT_MODE_DEVELOPER_INSTRUCTIONS)};
    if (message.params?.collaborationMode?.mode !== expectedCollaborationMode) process.exit(6);
    if (message.params?.collaborationMode?.settings?.model !== "gpt-test") process.exit(7);
    if (message.params?.collaborationMode?.settings?.developer_instructions !== expectedDeveloperInstructions) process.exit(8);
    if (scenario === "resume-rebuild") {
      const prompt = message.params?.input?.[0]?.text ?? "";
      if (!prompt.includes("<synara_resume_snapshot_json>") || !prompt.includes("Focused tests passed") || !prompt.includes("<current_user>\\ncontinue\\n</current_user>")) process.exit(3);
    }
    send({ id: message.id, result: { turn: { id: "turn-1", items: [], status: "inProgress", error: null } } });
    if (scenario === "approval") {
      send({ id: "approval-rpc", method: "item/commandExecution/requestApproval", params: { threadId: "thread-new", turnId: "turn-1", itemId: "command-1", command: "git push origin main", cwd: process.cwd(), reason: "Publish main" } });
    } else if (scenario === "file-change-approval") {
      send({ method: "item/started", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "file-change-1", type: "fileChange", status: "inProgress", changes: [{ path: "package.json", kind: { type: "add" }, diff: "{}" }] } } });
      send({ id: "file-approval-rpc", method: "item/fileChange/requestApproval", params: { threadId: "thread-new", turnId: "turn-1", itemId: "file-change-1", reason: "Add package manifest" } });
    } else if (scenario === "file-change-missing-item") {
      send({ id: "file-approval-missing-rpc", method: "item/fileChange/requestApproval", params: { threadId: "thread-new", turnId: "turn-1", itemId: "file-change-missing", reason: "Unclassified patch" } });
    } else if (scenario === "long-approval") {
      send({ id: longApprovalId, method: "item/commandExecution/requestApproval", params: { threadId: "thread-new", turnId: "turn-1", itemId: "command-long", command: "git status --short", cwd: process.cwd(), reason: "Run a status check" } });
    } else if (scenario === "user-input") {
      send({ id: "user-input-rpc", method: "item/tool/requestUserInput", params: { threadId: "thread-new", turnId: "turn-1", itemId: "input-1", autoResolutionMs: null, questions: [{ id: "environment", header: "Environment", question: "Where should this run?", isOther: false, isSecret: false, options: [{ label: "Staging", description: "Use staging." }] }] } });
    } else if (scenario === "resume") {
      complete("resumed");
    } else if (scenario === "resume-with-recovery-metadata") {
      const prompt = message.params?.input?.[0]?.text ?? "";
      if (!prompt.includes("<synara_agent_memory_json>")) process.exit(13);
      if (!prompt.includes('"memoryKey":"instructions"')) process.exit(14);
      if (!prompt.includes("<synara_resume_snapshot_json>")) process.exit(15);
      if (!prompt.includes('"requestId":"approval-suspended"')) process.exit(16);
      if (!prompt.includes("<current_user>\\ncontinue from the suspended approval\\n</current_user>")) process.exit(17);
      if (prompt.includes("<synara_transcript>")) process.exit(18);
      if (prompt.includes("prior native question") || prompt.includes("prior native answer")) process.exit(19);
      complete("resumed with metadata");
    } else if (scenario === "resume-rebuild") {
      complete("rebuilt");
    } else if (scenario === "interrupt") {
      send({ method: "item/agentMessage/delta", params: { threadId: "thread-new", turnId: "turn-1", itemId: "agent-1", delta: "waiting" } });
    } else if (scenario === "steer") {
      send({ method: "item/agentMessage/delta", params: { threadId: "thread-new", turnId: "turn-1", itemId: "agent-1", delta: "working" } });
    } else if (scenario === "proxy-output") {
      complete(process.env.HTTP_PROXY ?? "missing-proxy");
    } else if (scenario === "terminal") {
      send({ method: "item/started", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "command-terminal-1", type: "commandExecution", command: "bun run test", cwd: process.cwd(), status: "inProgress" } } });
      send({ method: "item/commandExecution/outputDelta", params: { threadId: "thread-new", turnId: "turn-1", itemId: "command-terminal-1", delta: "tests " } });
      send({ method: "item/commandExecution/outputDelta", params: { threadId: "thread-new", turnId: "turn-1", itemId: "command-terminal-1", delta: "passed\\n" } });
      send({ method: "item/completed", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "command-terminal-1", type: "commandExecution", command: "bun run test", cwd: process.cwd(), aggregatedOutput: "tests passed\\n", exitCode: 0, status: "completed" } } });
      complete("terminal complete");
    } else if (scenario === "terminal-after-turn") {
      send({ method: "item/started", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "command-terminal-late", type: "commandExecution", command: "bun run test", cwd: process.cwd(), status: "inProgress" } } });
      complete("terminal complete");
      send({ id: "late-terminal-approval", method: "item/commandExecution/requestApproval", params: { command: "late command" } });
      send({ method: "thread/tokenUsage/updated", params: { threadId: "thread-new", tokenUsage: { last: { inputTokens: 987654 } } } });
      send({ method: "warning", params: { message: "late terminal warning" } });
      setTimeout(() => {
        send({ method: "item/commandExecution/outputDelta", params: { threadId: "thread-new", turnId: "turn-1", itemId: "command-terminal-late", delta: "tests passed\\n" } });
        send({ method: "item/completed", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "command-terminal-late", type: "commandExecution", command: "bun run test", cwd: process.cwd(), aggregatedOutput: "tests passed\\n", exitCode: 0, status: "completed" } } });
      }, 2_250);
    } else if (scenario === "terminal-duplicate-completion") {
      send({ method: "item/started", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "command-terminal-duplicate", type: "commandExecution", command: "git push origin main", cwd: process.cwd(), status: "inProgress" } } });
      send({ method: "item/completed", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "command-terminal-duplicate", type: "commandExecution", command: "git push origin main", cwd: process.cwd(), aggregatedOutput: "", status: "declined" } } });
      send({ method: "item/completed", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "command-terminal-duplicate", type: "commandExecution", command: "git push origin main", cwd: process.cwd(), aggregatedOutput: "late duplicate output", exitCode: 0, status: "completed" } } });
      complete("command declined");
    } else if (scenario === "terminal-missing-completion") {
      send({ method: "item/started", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "command-terminal-missing", type: "commandExecution", command: "bun run test", cwd: process.cwd(), status: "inProgress" } } });
      complete("terminal incomplete");
    } else if (scenario === "generated-file") {
      const path = require("node:path");
      const generatedDirectory = path.join(process.cwd(), ".synara-stage3-acceptance");
      const generatedPath = path.join(generatedDirectory, "generated-file.txt");
      fs.mkdirSync(generatedDirectory, { recursive: true });
      fs.writeFileSync(generatedPath, "generated by Codex\\n");
      send({ method: "item/completed", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "file-change-1", type: "fileChange", status: "completed", changes: [{ path: generatedPath, kind: { type: "add" }, diff: "generated" }, { path: generatedPath, kind: { type: "update", move_path: null }, diff: "updated" }] } } });
      complete("file generated");
    } else if (scenario === "large-diff") {
      const diff = ["diff --git a/large.txt b/large.txt", "--- a/large.txt", "+++ b/large.txt", "@@ -1,1 +1,5000 @@", "-before", ...Array.from({ length: 5000 }, (_, index) => "+after-" + index + "-" + "x".repeat(16)), ""].join("\\n");
      send({ method: "turn/diff/updated", params: { threadId: "thread-new", turnId: "turn-1", diff } });
      complete("large diff ready");
    } else if (scenario === "credential-environment") {
      complete("credential isolated");
    } else if (scenario === "ambient-hook") {
      complete("ambient hook isolated");
    }
  } else if (message.method === "turn/steer") {
    if (message.params?.expectedTurnId !== "turn-1" || message.params?.input?.[0]?.text !== "focus on the failing test") process.exit(2);
    send({ id: message.id, result: {} });
    complete("steered");
  } else if (message.method === "turn/interrupt") {
    send({ id: message.id, result: {} });
    send({ method: "turn/completed", params: { threadId: "thread-new", turn: { id: "turn-1", items: [], status: "interrupted", error: null } } });
  } else if (message.id === "approval-rpc") {
    if (message.result?.decision !== "accept") process.exit(2);
    send({ method: "thread/tokenUsage/updated", params: { threadId: "thread-new", turnId: "turn-1", tokenUsage: { total: {}, last: { totalTokens: 8, inputTokens: 5, cachedInputTokens: 1, outputTokens: 3, reasoningOutputTokens: 0 }, modelContextWindow: 100 } } });
    complete("approved");
  } else if (message.id === "file-approval-rpc") {
    if (message.result?.decision !== "decline") process.exit(2);
    send({ method: "item/completed", params: { threadId: "thread-new", turnId: "turn-1", item: { id: "file-change-1", type: "fileChange", status: "declined", changes: [{ path: "package.json", kind: { type: "add" }, diff: "{}" }] } } });
    complete("patch declined");
  } else if (message.id === longApprovalId) {
    if (message.result?.decision !== "accept") process.exit(2);
    complete("approved");
  } else if (message.id === "user-input-rpc") {
    complete(message.result?.answers?.environment?.answers?.[0] ?? "missing");
  }
});
`;
}
