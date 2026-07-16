import type {
  Options as ClaudeQueryOptions,
  SDKMessage,
  SDKUserMessage,
} from "@anthropic-ai/claude-agent-sdk";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import type { ClaudeQueryFactory, ClaudeQueryRuntime } from "./claudeAgentSdkRuntime";
import { startProviderHostRun, type RunnerInput, type RunnerMessage } from "./providerHost";

describe("Claude Agent SDK runtime", () => {
  it("stores an oversized native tool Diff as a Runtime Output ArtifactCandidate", async () => {
    const testDirectory = mkdtempSync(join(tmpdir(), "synara-claude-large-diff-"));
    const canonicalWorkspaceDirectory = join(testDirectory, "workspace");
    const workspaceDirectory = join(testDirectory, "workspace-alias");
    mkdirSync(canonicalWorkspaceDirectory);
    symlinkSync(
      canonicalWorkspaceDirectory,
      workspaceDirectory,
      process.platform === "win32" ? "junction" : "dir",
    );
    writeFileSync(join(canonicalWorkspaceDirectory, "large.txt"), "after\n");
    writeFileSync(join(canonicalWorkspaceDirectory, "omitted-large.txt"), "after\n");
    writeFileSync(join(canonicalWorkspaceDirectory, "omitted-edit.txt"), "after-edit\n");
    const messages: RunnerMessage[] = [];
    try {
      const patch = [
        "diff --git a/large.txt b/large.txt",
        "--- a/large.txt",
        "+++ b/large.txt",
        "@@ -1,1 +1,5000 @@",
        "-before",
        ...Array.from({ length: 5_000 }, (_, index) => `+after-${index}-${"x".repeat(16)}`),
        "",
      ].join("\n");
      const omittedPatchBefore = Array.from(
        { length: 5_000 },
        (_, index) => `before-${index}-${"y".repeat(16)}`,
      ).join("\n");
      const omittedPatch = [
        "diff --git a/omitted-large.txt b/omitted-large.txt",
        "--- a/omitted-large.txt",
        "+++ b/omitted-large.txt",
        "@@ -1,5000 +1,1 @@",
        ...omittedPatchBefore.split("\n").map((line) => `-${line}`),
        "+after",
        "",
      ].join("\n");
      const editPatch = [
        "diff --git a/omitted-edit.txt b/omitted-edit.txt",
        "--- a/omitted-edit.txt",
        "+++ b/omitted-edit.txt",
        "@@ -1,1 +1,1 @@",
        "-before-edit",
        "+after-edit",
        "",
      ].join("\n");
      const queryFactory: ClaudeQueryFactory = ({ options }) =>
        fakeQuery(
          (async function* () {
            const postToolUse = requiredOptions(options).hooks?.PostToolUse?.[0]?.hooks[0];
            await postToolUse?.(
              {
                hook_event_name: "PostToolUse",
                tool_name: "Write",
                tool_input: {
                  file_path: join(workspaceDirectory, "large.txt"),
                  content: "after\n",
                },
                tool_response: {
                  type: "update",
                  filePath: join(canonicalWorkspaceDirectory, "large.txt"),
                  content: "after\n",
                  originalFile: "before\n",
                  structuredPatch: [],
                  gitDiff: {
                    filename: "large.txt",
                    status: "modified",
                    additions: 5_000,
                    deletions: 1,
                    changes: 5_001,
                    patch,
                  },
                },
                tool_use_id: "write-large-diff",
              } as never,
              undefined,
              { signal: new AbortController().signal },
            );
            await postToolUse?.(
              {
                hook_event_name: "PostToolUse",
                tool_name: "Write",
                tool_input: {
                  file_path: join(workspaceDirectory, "omitted-large.txt"),
                  content: "after\n",
                },
                tool_response: {
                  type: "update",
                  filePath: join(canonicalWorkspaceDirectory, "omitted-large.txt"),
                  content: "after\n",
                  originalFile: `${omittedPatchBefore}\n`,
                  structuredPatch: [],
                },
                tool_use_id: "write-omitted-large-diff",
              } as never,
              undefined,
              { signal: new AbortController().signal },
            );
            await postToolUse?.(
              {
                hook_event_name: "PostToolUse",
                tool_name: "Edit",
                tool_input: {
                  file_path: join(workspaceDirectory, "omitted-edit.txt"),
                  old_string: "before-edit",
                  new_string: "after-edit",
                },
                tool_response: {
                  filePath: join(canonicalWorkspaceDirectory, "omitted-edit.txt"),
                  oldString: "before-edit",
                  newString: "after-edit",
                  originalFile: "before-edit\n",
                  structuredPatch: [],
                  replaceAll: false,
                },
                tool_use_id: "edit-omitted-diff",
              } as never,
              undefined,
              { signal: new AbortController().signal },
            );
            yield sdkMessage(systemInit("session-large-diff", "claude-test"));
            yield sdkMessage(successResult("session-large-diff", "done", {}));
          })(),
        );

      const run = startProviderHostRun(
        claudeInput({
          inputText: "produce a large diff",
          workspaceDirectory,
          runtimeOutputDirectory: workspaceDirectory,
        }),
        null,
        (message) => messages.push(message),
        { claudeQueryFactory: queryFactory },
      );

      await expect(run.result).resolves.toMatchObject({ output: { text: "done" } });
      const diffArtifact = messages.find(
        (message) => message.type === "artifact" && message.artifact.kind === "diff",
      );
      expect(diffArtifact).toMatchObject({
        type: "artifact",
        artifact: {
          kind: "diff",
          sourceRoot: "runtime-output",
          contentType: "text/x-diff; charset=utf-8",
          fileCount: 3,
          additions: 5_002,
          deletions: 5_002,
        },
      });
      if (diffArtifact?.type !== "artifact") throw new Error("Expected a Diff ArtifactCandidate.");
      expect(readFileSync(join(workspaceDirectory, diffArtifact.artifact.path), "utf8")).toBe(
        `${patch}\n${omittedPatch}\n${editPatch}`,
      );
      expect(JSON.stringify(messages)).not.toContain(workspaceDirectory);
    } finally {
      rmSync(testDirectory, { recursive: true, force: true });
    }
  });

  it("emits one standalone generated-file candidate from a successful native Write tool", async () => {
    const workspaceDirectory = mkdtempSync(join(tmpdir(), "synara-claude-generated-file-"));
    const generatedDirectory = join(workspaceDirectory, ".synara-stage3-acceptance");
    const generatedPath = join(generatedDirectory, "generated-file.txt");
    const messages: RunnerMessage[] = [];
    try {
      const queryFactory: ClaudeQueryFactory = ({ options }) =>
        fakeQuery(
          (async function* () {
            const hooks = requiredOptions(options).hooks;
            mkdirSync(generatedDirectory, { recursive: true });
            writeFileSync(generatedPath, "generated by Claude\n");
            const postToolUse = hooks?.PostToolUse?.[0]?.hooks[0];
            await postToolUse?.(
              {
                hook_event_name: "PostToolUse",
                tool_name: "Write",
                tool_input: { file_path: generatedPath, content: "generated by Claude\n" },
                tool_response: { success: true },
                tool_use_id: "write-generated-file",
              } as never,
              undefined,
              { signal: new AbortController().signal },
            );
            yield sdkMessage(systemInit("session-generated-file", "claude-test"));
            yield sdkMessage(successResult("session-generated-file", "done", {}));
          })(),
        );

      const run = startProviderHostRun(
        claudeInput({ inputText: "generate a file", workspaceDirectory }),
        null,
        (message) => messages.push(message),
        { claudeQueryFactory: queryFactory },
      );

      await expect(run.result).resolves.toMatchObject({ output: { text: "done" } });
      expect(messages.filter((message) => message.type === "artifact")).toEqual([
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
      expect(JSON.stringify(messages)).not.toContain(workspaceDirectory);
    } finally {
      rmSync(workspaceDirectory, { recursive: true, force: true });
    }
  });

  it("binds the agentd-created Runtime Output Root for controlled Claude credentials", async () => {
    const runtimeOutputDirectory = "/tmp/synara-claude-runtime-output";
    const queryFactory: ClaudeQueryFactory = ({ options }) =>
      fakeQuery(
        (async function* () {
          const environment = requiredOptions(options).env;
          expect(environment?.CLAUDE_CONFIG_DIR).toBe(runtimeOutputDirectory);
          if (process.platform === "win32") {
            expect(environment?.CLAUDE_SECURESTORAGE_CONFIG_DIR).toBe(runtimeOutputDirectory);
          } else {
            expect(environment?.CLAUDE_SECURESTORAGE_CONFIG_DIR).toBeUndefined();
          }
          yield sdkMessage(systemInit("session-runtime-output", "claude-test"));
          yield sdkMessage(successResult("session-runtime-output", "done", {}));
        })(),
      );

    const run = startProviderHostRun(
      claudeInput({ inputText: "use controlled output", runtimeOutputDirectory }),
      { payload: { apiKey: "provider-secret" } },
      () => {},
      { claudeQueryFactory: queryFactory },
    );

    await expect(run.result).resolves.toMatchObject({ output: { text: "done" } });
  });

  it("preserves the user Claude config path for ambient OAuth authentication", async () => {
    const runtimeOutputDirectory = "/tmp/synara-claude-runtime-output";
    const queryFactory: ClaudeQueryFactory = ({ options }) =>
      fakeQuery(
        (async function* () {
          const environment = requiredOptions(options).env;
          expect(environment?.HOME).toBe("/home/worker");
          expect(environment?.CLAUDE_CONFIG_DIR).toBeUndefined();
          expect(environment?.CLAUDE_SECURESTORAGE_CONFIG_DIR).toBeUndefined();
          yield sdkMessage(systemInit("session-ambient-oauth", "claude-test"));
          yield sdkMessage(successResult("session-ambient-oauth", "done", {}));
        })(),
      );

    const run = startProviderHostRun(
      claudeInput({ inputText: "use ambient OAuth", runtimeOutputDirectory }),
      null,
      () => {},
      { claudeQueryFactory: queryFactory, environment: { HOME: "/home/worker" } },
    );

    await expect(run.result).resolves.toMatchObject({ output: { text: "done" } });
  });

  it.each(["persistedOutputPath", "rawOutputPath"] as const)(
    "emits a controlled %s as a terminal Artifact without duplicate inline output",
    async (pathField) => {
      const messages: RunnerMessage[] = [];
      const runtimeOutputDirectory = "/tmp/synara-claude-runtime-output";
      const outputPath = `${runtimeOutputDirectory}/tool-results/tool-1.log`;
      const queryFactory: ClaudeQueryFactory = () =>
        fakeQuery(
          (async function* () {
            yield sdkMessage(systemInit("session-controlled-output", "claude-test"));
            yield sdkMessage({
              type: "assistant",
              session_id: "session-controlled-output",
              message: {
                content: [
                  {
                    type: "tool_use",
                    id: "tool-controlled-output",
                    name: "Bash",
                    input: { command: "produce lots of output" },
                  },
                ],
              },
            });
            yield sdkMessage({
              type: "user",
              session_id: "session-controlled-output",
              tool_use_result: {
                stdout: "duplicate inline output",
                stderr: "",
                interrupted: false,
                [pathField]: outputPath,
                persistedOutputSize: 4_096,
              },
              message: {
                content: [
                  {
                    type: "tool_result",
                    tool_use_id: "tool-controlled-output",
                    content: "duplicate inline output",
                  },
                ],
              },
            });
            yield sdkMessage(successResult("session-controlled-output", "done", {}));
          })(),
        );

      const run = startProviderHostRun(
        claudeInput({ inputText: "capture output", runtimeOutputDirectory }),
        null,
        (message) => messages.push(message),
        { claudeQueryFactory: queryFactory },
      );

      await expect(run.result).resolves.toMatchObject({ output: { text: "done" } });
      expect(messages).toContainEqual({
        type: "artifact",
        artifact: {
          path: "tool-results/tool-1.log",
          kind: "terminal_log",
          originalName: "claude-terminal.log",
          contentType: "text/plain",
          sourceRoot: "runtime-output",
          terminalId: "tool-controlled-output",
          encoding: "utf-8",
          ...(pathField === "persistedOutputPath" ? { reportedSize: 4_096 } : {}),
        },
      });
      expect(messages).not.toContainEqual(
        expect.objectContaining({ eventType: "runtime.command.output" }),
      );
      expect(messages).not.toContainEqual(
        expect.objectContaining({ eventType: "runtime.provider.warning" }),
      );
      expect(JSON.stringify(messages)).not.toContain(runtimeOutputDirectory);
    },
  );

  it("emits a controlled background output_file once without duplicating its summary", async () => {
    const messages: RunnerMessage[] = [];
    const runtimeOutputDirectory = "/tmp/synara-claude-runtime-output";
    const outputPath = `${runtimeOutputDirectory}/tasks/background-1.log`;
    const queryFactory: ClaudeQueryFactory = () =>
      fakeQuery(
        (async function* () {
          yield sdkMessage(systemInit("session-background-output", "claude-test"));
          yield sdkMessage({
            type: "assistant",
            session_id: "session-background-output",
            message: {
              content: [
                {
                  type: "tool_use",
                  id: "tool-background-output",
                  name: "Bash",
                  input: { command: "run in background" },
                },
              ],
            },
          });
          yield sdkMessage({
            type: "user",
            session_id: "session-background-output",
            tool_use_result: {
              stdout: "",
              stderr: "",
              interrupted: false,
              backgroundTaskId: "background-1",
            },
            message: {
              content: [
                {
                  type: "tool_result",
                  tool_use_id: "tool-background-output",
                  content: "background task started",
                },
              ],
            },
          });
          yield sdkMessage({
            type: "system",
            subtype: "task_notification",
            session_id: "session-background-output",
            tool_use_id: "tool-background-output",
            status: "completed",
            summary: "duplicate background summary",
            output_file: outputPath,
          });
          yield sdkMessage(successResult("session-background-output", "done", {}));
        })(),
      );

    const run = startProviderHostRun(
      claudeInput({ inputText: "capture background output", runtimeOutputDirectory }),
      null,
      (message) => messages.push(message),
      { claudeQueryFactory: queryFactory },
    );

    await expect(run.result).resolves.toMatchObject({ output: { text: "done" } });
    expect(messages.filter((message) => message.type === "artifact")).toEqual([
      {
        type: "artifact",
        artifact: {
          path: "tasks/background-1.log",
          kind: "terminal_log",
          originalName: "claude-terminal.log",
          contentType: "text/plain",
          sourceRoot: "runtime-output",
          terminalId: "tool-background-output",
          encoding: "utf-8",
        },
      },
    ]);
    expect(messages).not.toContainEqual(
      expect.objectContaining({ eventType: "runtime.command.output" }),
    );
    expect(JSON.stringify(messages)).not.toContain(runtimeOutputDirectory);
  });

  it.each([
    {
      label: "an escaping path",
      runtimeOutputDirectory: "/tmp/synara-claude-runtime-output",
      outputPath: "/tmp/outside-runtime-output.log",
    },
    {
      label: "a path without a controlled root",
      runtimeOutputDirectory: undefined,
      outputPath: "/tmp/synara-claude-runtime-output/tool-results/unbound.log",
    },
  ])("keeps inline output and warns safely for $label", async (testCase) => {
    const messages: RunnerMessage[] = [];
    const queryFactory: ClaudeQueryFactory = () =>
      fakeQuery(
        (async function* () {
          yield sdkMessage(systemInit("session-rejected-output", "claude-test"));
          yield sdkMessage({
            type: "assistant",
            session_id: "session-rejected-output",
            message: {
              content: [
                {
                  type: "tool_use",
                  id: "tool-rejected-output",
                  name: "Bash",
                  input: { command: "produce output" },
                },
              ],
            },
          });
          yield sdkMessage({
            type: "user",
            session_id: "session-rejected-output",
            tool_use_result: {
              stdout: "safe inline output",
              stderr: "",
              interrupted: false,
              persistedOutputPath: testCase.outputPath,
            },
            message: {
              content: [
                {
                  type: "tool_result",
                  tool_use_id: "tool-rejected-output",
                  content: "safe inline output",
                },
              ],
            },
          });
          yield sdkMessage(successResult("session-rejected-output", "done", {}));
        })(),
      );

    const run = startProviderHostRun(
      claudeInput({
        inputText: "reject unsafe path",
        ...(testCase.runtimeOutputDirectory
          ? { runtimeOutputDirectory: testCase.runtimeOutputDirectory }
          : {}),
      }),
      null,
      (message) => messages.push(message),
      { claudeQueryFactory: queryFactory },
    );

    await expect(run.result).resolves.toMatchObject({ output: { text: "done" } });
    expect(messages).not.toContainEqual(expect.objectContaining({ type: "artifact" }));
    expect(messages).toContainEqual(
      expect.objectContaining({
        type: "event",
        eventType: "runtime.command.output",
        payload: expect.objectContaining({ text: "safe inline output" }),
      }),
    );
    expect(messages).toContainEqual(
      expect.objectContaining({
        type: "event",
        eventType: "runtime.provider.warning",
      }),
    );
    expect(JSON.stringify(messages)).not.toContain(testCase.outputPath);
  });

  it("delivers approval, safe tool lifecycle, usage and streamed output", async () => {
    const messages: RunnerMessage[] = [];
    const interaction = nextInteraction(messages, "approval");
    const queryFactory: ClaudeQueryFactory = ({ prompt, options }) =>
      fakeQuery(
        (async function* () {
          expect(await promptText(prompt)).toBe("run status");
          const queryOptions = requiredOptions(options);
          expect(queryOptions.permissionMode).toBe("default");
          const preToolUse = queryOptions.hooks?.PreToolUse?.[0]?.hooks[0];
          expect(
            await preToolUse?.(
              {
                hook_event_name: "PreToolUse",
                tool_name: "Bash",
                tool_input: { command: "git status --short" },
              } as never,
              "tool-approval",
              { signal: new AbortController().signal },
            ),
          ).toMatchObject({
            hookSpecificOutput: {
              hookEventName: "PreToolUse",
              permissionDecision: "ask",
            },
          });
          yield sdkMessage(systemInit("session-approval", "claude-test"));
          yield sdkMessage({
            type: "assistant",
            session_id: "session-approval",
            message: {
              content: [
                {
                  type: "tool_use",
                  id: "tool-approval",
                  name: "Bash",
                  input: { command: "git status --short", hiddenToken: "do-not-project" },
                },
              ],
            },
          });
          const decision = await queryOptions.canUseTool?.(
            "Bash",
            { command: "git status --short", hiddenToken: "do-not-project" },
            {
              signal: new AbortController().signal,
              toolUseID: "tool-approval",
              requestId: "permission-tool-approval",
              title: "Run repository status",
            },
          );
          expect(decision).toMatchObject({ behavior: "allow" });
          yield sdkMessage({
            type: "user",
            session_id: "session-approval",
            tool_use_result: { stdout: "clean", stderr: "", interrupted: false },
            message: {
              content: [{ type: "tool_result", tool_use_id: "tool-approval", content: "clean" }],
            },
          });
          yield sdkMessage({
            type: "assistant",
            session_id: "session-approval",
            message: { content: [{ type: "text", text: "approved" }] },
          });
          yield sdkMessage(
            successResult("session-approval", "approved", {
              input_tokens: 4,
              output_tokens: 2,
            }),
          );
        })(),
      );

    const run = startProviderHostRun(
      claudeInput({
        inputText: "run status",
        runtimeMode: "approval-required",
      }),
      null,
      (message) => messages.push(message),
      { claudeQueryFactory: queryFactory },
    );

    const request = await interaction;
    expect(request.payload).toMatchObject({
      requestId: "claude:approval:tool-approval",
      provider: "claudeAgent",
      requestKind: "command",
      command: "git status --short",
      cwd: "/tmp/synara-claude-runtime",
    });
    expect(JSON.stringify(request.payload)).not.toContain("do-not-project");
    await run.resolveApproval?.({
      requestId: request.payload.requestId,
      resolution: { decision: "accept" },
    });

    await expect(run.result).resolves.toMatchObject({
      output: { provider: "claudeAgent", model: "claude-test", text: "approved" },
      providerResumeCursor: "session-approval",
    });
    expect(messages).toEqual(
      expect.arrayContaining([
        {
          type: "event",
          eventType: "runtime.provider.activity",
          payload: expect.objectContaining({
            provider: "claudeAgent",
            itemType: "Bash",
            status: "started",
            itemId: "tool-approval",
            terminalId: "tool-approval",
            terminalEventType: "terminal.started",
            commandSummary: "git status --short",
            cwdLabel: ".",
          }),
        },
        {
          type: "event",
          eventType: "runtime.command.output",
          payload: {
            provider: "claudeAgent",
            terminalId: "tool-approval",
            encoding: "utf-8",
            text: "clean",
            byteOffset: 0,
            byteLength: 5,
          },
        },
        {
          type: "event",
          eventType: "runtime.provider.activity",
          payload: expect.objectContaining({
            provider: "claudeAgent",
            itemType: "Bash",
            status: "completed",
            itemId: "tool-approval",
            terminalId: "tool-approval",
            terminalEventType: "terminal.exited",
          }),
        },
        {
          type: "event",
          eventType: "runtime.usage",
          payload: { provider: "claudeAgent", input_tokens: 4, output_tokens: 2 },
        },
      ]),
    );
  });

  it("answers AskUserQuestion in native Plan Mode", async () => {
    const messages: RunnerMessage[] = [];
    const interaction = nextInteraction(messages, "user-input");
    const queryFactory: ClaudeQueryFactory = ({ prompt, options }) =>
      fakeQuery(
        (async function* () {
          expect(await promptText(prompt)).toBe("make a deployment plan");
          const queryOptions = requiredOptions(options);
          expect(queryOptions.permissionMode).toBe("plan");
          const decision = await queryOptions.canUseTool?.(
            "AskUserQuestion",
            {
              questions: [
                {
                  header: "Environment",
                  question: "Where should this run?",
                  options: [
                    { label: "Staging", description: "Use staging." },
                    { label: "Production", description: "Use production." },
                  ],
                  multiSelect: false,
                },
              ],
            },
            {
              signal: new AbortController().signal,
              toolUseID: "tool-question",
              requestId: "permission-tool-question",
            },
          );
          expect(decision).toMatchObject({
            behavior: "allow",
            updatedInput: {
              answers: { "Where should this run?": "Staging" },
            },
          });
          yield sdkMessage(systemInit("session-plan", "claude-test"));
          yield sdkMessage(successResult("session-plan", "INPUT_OK:Staging", {}));
        })(),
      );

    const run = startProviderHostRun(
      claudeInput({
        inputText: "make a deployment plan",
        runtimeMode: "approval-required",
        interactionMode: "plan",
      }),
      null,
      (message) => messages.push(message),
      { claudeQueryFactory: queryFactory },
    );

    const request = await interaction;
    expect(request.payload).toMatchObject({
      requestId: "claude:user-input:tool-question",
      questions: [
        {
          id: "question-1",
          header: "Environment",
          question: "Where should this run?",
        },
      ],
    });
    await run.resolveUserInput?.({
      requestId: request.payload.requestId,
      resolution: { answers: { "question-1": "Staging" } },
    });
    await expect(run.result).resolves.toMatchObject({
      output: { text: "INPUT_OK:Staging" },
      providerResumeCursor: "session-plan",
    });
  });

  it("scopes approval and user-input request IDs to the Execution Generation", async () => {
    const messages: RunnerMessage[] = [];
    const approvalInteractions = nextInteractions(messages, "approval", 2);
    const userInputInteraction = nextInteraction(messages, "user-input");
    const longApprovalId = `stable-approval-${"x".repeat(300)}`;
    const queryFactory: ClaudeQueryFactory = ({ prompt, options }) =>
      fakeQuery(
        (async function* () {
          expect(await promptText(prompt)).toBe("exercise generated interactions");
          const queryOptions = requiredOptions(options);
          const approval = queryOptions.canUseTool?.(
            "Bash",
            { command: "git status --short" },
            {
              signal: new AbortController().signal,
              toolUseID: longApprovalId,
              requestId: "permission-long-approval-1",
            },
          );
          const duplicateApproval = queryOptions.canUseTool?.(
            "Bash",
            { command: "git diff --stat" },
            {
              signal: new AbortController().signal,
              toolUseID: longApprovalId,
              requestId: "permission-long-approval-2",
            },
          );
          await expect(Promise.all([approval, duplicateApproval])).resolves.toEqual([
            expect.objectContaining({ behavior: "allow" }),
            expect.objectContaining({ behavior: "allow" }),
          ]);
          const userInput = await queryOptions.canUseTool?.(
            "AskUserQuestion",
            {
              questions: [
                {
                  header: "Environment",
                  question: "Where should this run?",
                  options: [{ label: "Staging", description: "Use staging." }],
                  multiSelect: false,
                },
              ],
            },
            {
              signal: new AbortController().signal,
              toolUseID: "stable-user-input",
              requestId: "permission-stable-user-input",
            },
          );
          expect(userInput).toMatchObject({
            behavior: "allow",
            updatedInput: { answers: { "Where should this run?": "Staging" } },
          });
          yield sdkMessage(systemInit("session-generated-interactions", "claude-test"));
          yield sdkMessage(successResult("session-generated-interactions", "done", {}));
        })(),
      );

    const run = startProviderHostRun(
      claudeInput({
        inputText: "exercise generated interactions",
        runtimeMode: "approval-required",
        generation: 9,
      }),
      null,
      (message) => messages.push(message),
      { claudeQueryFactory: queryFactory },
    );

    const approvals = await approvalInteractions;
    const approvalRequestIds = approvals.map((approval) => String(approval.payload.requestId));
    expect(approvalRequestIds).toHaveLength(2);
    expect(new Set(approvalRequestIds).size).toBe(2);
    expect(approvalRequestIds[0]).toMatch(/^claude:generation-9:approval:/);
    expect(approvalRequestIds[1]).toMatch(/:1$/);
    for (const requestId of approvalRequestIds) {
      expect(Buffer.byteLength(requestId)).toBeLessThanOrEqual(200);
      await run.resolveApproval?.({
        requestId,
        resolution: { decision: "accept" },
      });
    }

    const userInput = await userInputInteraction;
    expect(userInput.payload.requestId).toBe("claude:generation-9:user-input:stable-user-input");
    await run.resolveUserInput?.({
      requestId: userInput.payload.requestId,
      resolution: { answers: { "question-1": "Staging" } },
    });

    await expect(run.result).resolves.toMatchObject({
      output: { text: "done" },
      providerResumeCursor: "session-generated-interactions",
    });
  });

  it("prefers native resume even when authoritative history is present", async () => {
    let calls = 0;
    const queryFactory: ClaudeQueryFactory = ({ prompt, options }) =>
      fakeQuery(
        (async function* () {
          calls += 1;
          expect(requiredOptions(options).resume).toBe("session-existing");
          expect(await promptText(prompt)).toBe("continue");
          yield sdkMessage(systemInit("session-existing", "claude-test"));
          yield sdkMessage(successResult("session-existing", "resumed", {}));
        })(),
      );
    const run = startProviderHostRun(
      {
        ...claudeInput({ inputText: "continue" }),
        providerResumeCursor: "session-existing",
        workload: {
          provider: "claudeAgent",
          inputText: "continue",
          conversationHistory: [
            { role: "user", text: "first" },
            { role: "assistant", text: "response" },
          ],
        },
      },
      null,
      () => {},
      { claudeQueryFactory: queryFactory },
    );

    await expect(run.result).resolves.toMatchObject({
      output: { text: "resumed" },
      providerResumeCursor: "session-existing",
    });
    expect(calls).toBe(1);
  });

  it("falls back to bounded authoritative history when native resume is invalid", async () => {
    const prompts: string[] = [];
    const messages: RunnerMessage[] = [];
    let calls = 0;
    const queryFactory: ClaudeQueryFactory = ({ prompt, options }) => {
      calls += 1;
      const attempt = calls;
      return fakeQuery(
        (async function* () {
          prompts.push(await promptText(prompt));
          if (attempt === 1) {
            expect(requiredOptions(options).resume).toBe("session-invalid");
            throw new Error("Session session-invalid expired: native-resume-secret");
          }
          expect(requiredOptions(options).resume).toBeUndefined();
          yield sdkMessage(systemInit("session-rebuilt", "claude-test"));
          yield sdkMessage(successResult("session-rebuilt", "rebuilt", {}));
        })(),
      );
    };
    const run = startProviderHostRun(
      {
        ...claudeInput({ inputText: "continue" }),
        providerResumeCursor: "session-invalid",
        workload: {
          provider: "claudeAgent",
          inputText: "continue",
          resumeSnapshot: {
            version: 1,
            sessionId: "session-1",
            turnId: "turn-2",
            provider: "claudeAgent",
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
      { claudeQueryFactory: queryFactory },
    );

    await expect(run.result).resolves.toMatchObject({
      output: { text: "rebuilt" },
      providerResumeCursor: "session-rebuilt",
    });
    expect(prompts[0]).toBe("continue");
    expect(prompts[1]).toContain("<assistant>\nresponse\n</assistant>");
    expect(prompts[1]).toContain("<synara_resume_snapshot_json>");
    expect(prompts[1]).toContain("Focused tests passed");
    expect(prompts[1]).toContain("<current_user>\ncontinue\n</current_user>");
    expect(messages).toContainEqual({
      type: "event",
      eventType: "runtime.provider.warning",
      payload: {
        provider: "claudeAgent",
        message:
          "Native Claude resume failed before turn activity; authoritative-history fallback selected.",
        kind: "session_resume",
        attemptedStrategy: "native-cursor",
        selectedStrategy: "authoritative-history",
        outcome: "fallback_selected",
        reasonCode: "session_resume_expired",
        fallbackSafety: "before_turn_activity",
        authoritativeHistorySequence: 23,
      },
    });
    expect(JSON.stringify(messages)).not.toContain("native-resume-secret");
    expect(JSON.stringify(messages)).not.toContain("session-invalid");
  });

  it("does not rebuild from history for a native resume rate-limit failure", async () => {
    const messages: RunnerMessage[] = [];
    let calls = 0;
    const queryFactory: ClaudeQueryFactory = ({ prompt, options }) => {
      calls += 1;
      return fakeQuery(
        (async function* () {
          await promptText(prompt);
          expect(requiredOptions(options).resume).toBe("session-rate-limited");
          throw new Error("Rate limit exceeded while resuming session");
        })(),
      );
    };
    const run = startProviderHostRun(
      {
        ...claudeInput({ inputText: "continue" }),
        providerResumeCursor: "session-rate-limited",
        workload: {
          provider: "claudeAgent",
          inputText: "continue",
          resumeSnapshot: {
            version: 1,
            sessionId: "session-1",
            turnId: "turn-2",
            provider: "claudeAgent",
            messages: [{ role: "assistant", text: "authoritative response" }],
            authoritativeHistorySequence: 24,
          },
        },
      },
      null,
      (message) => messages.push(message),
      { claudeQueryFactory: queryFactory },
    );

    await expect(run.result).rejects.toThrow("Rate limit exceeded");
    expect(calls).toBe(1);
    expect(messages).not.toContainEqual(
      expect.objectContaining({ eventType: "runtime.provider.warning" }),
    );
  });

  it.each([
    {
      errorStatus: 401,
      error: "authentication_failed",
      expected: "authentication failed with HTTP 401",
    },
    {
      errorStatus: 429,
      error: "rate_limit",
      expected: "rate limit exceeded with HTTP 429",
    },
  ])("fails fast on a stable SDK API retry: $error", async ({ errorStatus, error, expected }) => {
    let continuedAfterRetry = false;
    const queryFactory: ClaudeQueryFactory = ({ prompt }) =>
      fakeQuery(
        (async function* () {
          await promptText(prompt);
          yield sdkMessage({
            type: "system",
            subtype: "api_retry",
            attempt: 1,
            max_retries: 10,
            retry_delay_ms: 500,
            error_status: errorStatus,
            error,
            uuid: "retry-1",
            session_id: "session-api-retry",
          });
          continuedAfterRetry = true;
          yield sdkMessage(successResult("session-api-retry", "must not complete", {}));
        })(),
      );
    const run = startProviderHostRun(
      claudeInput({ inputText: "fail without hidden retries" }),
      null,
      () => {},
      { claudeQueryFactory: queryFactory },
    );

    await expect(run.result).rejects.toThrow(expected);
    expect(continuedAfterRetry).toBe(false);
  });

  it("classifies a contradictory SDK success result from its API error status", async () => {
    const queryFactory: ClaudeQueryFactory = () =>
      fakeQuery(
        (async function* () {
          yield sdkMessage({
            type: "result",
            subtype: "success",
            is_error: true,
            api_error_status: 401,
            errors: [],
            result: "",
            usage: {},
            session_id: "session-api-error-result",
          });
        })(),
      );
    const run = startProviderHostRun(
      claudeInput({ inputText: "surface terminal auth failure" }),
      null,
      () => {},
      { claudeQueryFactory: queryFactory },
    );

    await expect(run.result).rejects.toThrow("authentication failed with HTTP 401");
  });

  it("uses native query interrupt and rejects the active turn", async () => {
    let releaseInterrupt!: () => void;
    const interrupted = new Promise<void>((resolve) => {
      releaseInterrupt = resolve;
    });
    let nativeInterrupts = 0;
    let outputStarted!: () => void;
    const output = new Promise<void>((resolve) => {
      outputStarted = resolve;
    });
    const queryFactory: ClaudeQueryFactory = ({ prompt }) =>
      fakeQuery(
        (async function* () {
          await promptText(prompt);
          yield sdkMessage({
            type: "stream_event",
            session_id: "session-interrupt",
            event: {
              type: "content_block_delta",
              delta: { type: "text_delta", text: "started" },
            },
          });
          await interrupted;
          throw new Error("native query interrupted");
        })(),
        {
          onInterrupt: () => {
            nativeInterrupts += 1;
            releaseInterrupt();
          },
        },
      );
    const run = startProviderHostRun(
      claudeInput({ inputText: "long task" }),
      null,
      (message) => {
        if (message.type === "event" && message.eventType === "runtime.output.delta") {
          outputStarted();
        }
      },
      { claudeQueryFactory: queryFactory },
    );

    await output;
    run.interrupt();
    await expect(run.result).rejects.toThrow("interrupted");
    expect(nativeInterrupts).toBe(1);
  });

  it("steers the active Query through the native streaming input", async () => {
    let initialRead!: () => void;
    const initialReadPromise = new Promise<void>((resolve) => {
      initialRead = resolve;
    });
    const queryFactory: ClaudeQueryFactory = ({ prompt }) =>
      fakeQuery(
        (async function* () {
          if (typeof prompt === "string") throw new Error("Expected streaming Claude input.");
          const iterator = prompt[Symbol.asyncIterator]();
          const initial = await iterator.next();
          expect(messageText(initial.value)).toBe("long task");
          initialRead();
          const steered = await iterator.next();
          expect(messageText(steered.value)).toBe("focus on the failing test");
          expect(steered.value?.priority).toBe("now");
          yield sdkMessage(systemInit("session-steer", "claude-test"));
          yield sdkMessage(successResult("session-steer", "superseded", {}));
          yield sdkMessage(successResult("session-steer", "steered", {}));
        })(),
      );
    const run = startProviderHostRun(claudeInput({ inputText: "long task" }), null, () => {}, {
      claudeQueryFactory: queryFactory,
    });

    await initialReadPromise;
    await run.steer?.({ inputText: "focus on the failing test" });
    await expect(run.result).resolves.toMatchObject({
      output: { text: "steered" },
      providerResumeCursor: "session-steer",
    });
  });

  it("ignores a late approval resolution after steering cancels the Provider request", async () => {
    const messages: RunnerMessage[] = [];
    const interaction = nextInteraction(messages, "approval");
    let approvalCancelled!: () => void;
    const approvalCancelledPromise = new Promise<void>((resolve) => {
      approvalCancelled = resolve;
    });
    let releaseResult!: () => void;
    const resultReleased = new Promise<void>((resolve) => {
      releaseResult = resolve;
    });
    const queryFactory: ClaudeQueryFactory = ({ prompt, options }) =>
      fakeQuery(
        (async function* () {
          if (typeof prompt === "string") throw new Error("Expected streaming Claude input.");
          const iterator = prompt[Symbol.asyncIterator]();
          const initial = await iterator.next();
          expect(messageText(initial.value)).toBe("wait for approval");
          const controller = new AbortController();
          const canUseTool = requiredOptions(options).canUseTool;
          if (!canUseTool) throw new Error("Expected Claude permission callback.");
          const decision = canUseTool(
            "Bash",
            { command: "printf ok" },
            {
              signal: controller.signal,
              toolUseID: "tool-steer-cancelled",
              requestId: "permission-steer-cancelled",
            },
          );
          const steered = await iterator.next();
          expect(messageText(steered.value)).toBe("focus on the failing test");
          controller.abort();
          await expect(decision).resolves.toMatchObject({ behavior: "deny" });
          approvalCancelled();
          await resultReleased;
          yield sdkMessage(systemInit("session-steer-cancelled", "claude-test"));
          yield sdkMessage(successResult("session-steer-cancelled", "superseded", {}));
          yield sdkMessage(successResult("session-steer-cancelled", "steered", {}));
        })(),
      );
    const run = startProviderHostRun(
      claudeInput({ inputText: "wait for approval", runtimeMode: "approval-required" }),
      null,
      (message) => messages.push(message),
      { claudeQueryFactory: queryFactory },
    );

    const request = await interaction;
    await run.steer?.({ inputText: "focus on the failing test" });
    await approvalCancelledPromise;
    expect(() =>
      run.resolveApproval?.({
        requestId: request.payload.requestId,
        resolution: { decision: "accept" },
      }),
    ).not.toThrow();
    releaseResult();
    await expect(run.result).resolves.toMatchObject({
      output: { text: "steered" },
      providerResumeCursor: "session-steer-cancelled",
    });
  });

  it("cancels a pending durable approval when the Turn is interrupted", async () => {
    const messages: RunnerMessage[] = [];
    const interaction = nextInteraction(messages, "approval");
    let nativeInterrupts = 0;
    const queryFactory: ClaudeQueryFactory = ({ prompt, options }) =>
      fakeQuery(
        (async function* () {
          await promptText(prompt);
          yield sdkMessage({
            type: "assistant",
            session_id: "session-pending-approval",
            message: {
              content: [{ type: "tool_use", id: "tool-pending", name: "Bash", input: {} }],
            },
          });
          const decision = await requiredOptions(options).canUseTool?.(
            "Bash",
            { command: "sleep 30" },
            {
              signal: new AbortController().signal,
              toolUseID: "tool-pending",
              requestId: "permission-tool-pending",
            },
          );
          expect(decision).toMatchObject({ behavior: "deny" });
          throw new Error("interrupted after approval cancellation");
        })(),
        { onInterrupt: () => (nativeInterrupts += 1) },
      );
    const run = startProviderHostRun(
      claudeInput({ inputText: "wait for approval", runtimeMode: "approval-required" }),
      null,
      (message) => messages.push(message),
      { claudeQueryFactory: queryFactory },
    );

    await interaction;
    run.interrupt();
    await expect(run.result).rejects.toThrow("interrupted");
    expect(nativeInterrupts).toBe(1);
  });

  it("emulates review with a fixed read-only prompt and an immutable tool allowlist", async () => {
    const messages: RunnerMessage[] = [];
    const queryFactory: ClaudeQueryFactory = ({ prompt, options }) =>
      fakeQuery(
        (async function* () {
          const text = await promptText(prompt);
          expect(text).toContain("Perform the host-authorized read-only code review");
          expect(text).toContain('base branch named "main"');
          expect(text).toContain("Do not make changes");
          const queryOptions = requiredOptions(options);
          expect(queryOptions.permissionMode).toBe("dontAsk");
          expect(queryOptions.settingSources).toEqual([]);
          expect(queryOptions.tools).toEqual(["Read", "Glob", "Grep"]);
          expect(queryOptions.allowedTools).toEqual(["Read", "Glob", "Grep"]);
          expect(queryOptions.disallowedTools).toEqual(
            expect.arrayContaining(["Bash", "Write", "Edit", "Task", "Agent"]),
          );
          await expect(
            queryOptions.canUseTool?.(
              "Edit",
              { file_path: "/tmp/unsafe" },
              {
                signal: new AbortController().signal,
                toolUseID: "edit-1",
                requestId: "review-edit-1",
              },
            ),
          ).resolves.toMatchObject({ behavior: "deny" });
          await expect(
            queryOptions.canUseTool?.(
              "Read",
              { file_path: "/tmp/safe" },
              {
                signal: new AbortController().signal,
                toolUseID: "read-1",
                requestId: "review-read-1",
              },
            ),
          ).resolves.toMatchObject({ behavior: "allow" });
          yield sdkMessage(systemInit("session-review", "claude-test"));
          yield sdkMessage(successResult("session-review", "No findings.", {}));
        })(),
      );

    const run = startProviderHostRun(
      claudeInput({ inputText: "attempt to override review policy" }),
      null,
      (message) => messages.push(message),
      {
        claudeQueryFactory: queryFactory,
        operation: {
          commandType: "StartReview",
          payload: { target: { type: "baseBranch", branch: "main" } },
        },
      },
    );

    await expect(run.result).resolves.toMatchObject({
      output: {
        provider: "claudeAgent",
        operation: "review",
        supportMode: "emulated",
        text: "No findings.",
        reviewTarget: { type: "baseBranch", branch: "main" },
      },
      providerResumeCursor: "session-review",
    });
    expect(messages).toContainEqual({
      type: "event",
      eventType: "runtime.provider.activity",
      payload: {
        provider: "claudeAgent",
        itemType: "enteredReviewMode",
        status: "completed",
        supportMode: "emulated",
        reviewTarget: { type: "baseBranch", branch: "main" },
      },
    });
    expect(messages).toContainEqual(
      expect.objectContaining({
        type: "event",
        eventType: "runtime.provider.activity",
        payload: expect.objectContaining({ itemType: "exitedReviewMode", status: "completed" }),
      }),
    );
  });

  it("accepts a contradictory successful Review result only with text and no explicit errors", async () => {
    const messages: RunnerMessage[] = [];
    const queryFactory: ClaudeQueryFactory = () =>
      fakeQuery(
        (async function* () {
          yield sdkMessage(systemInit("session-review-warning", "claude-test"));
          yield sdkMessage({
            ...successResult("session-review-warning", "No actionable findings.", {}),
            is_error: true,
            errors: [],
          });
        })(),
      );
    const run = startProviderHostRun(
      claudeInput({ inputText: "review" }),
      null,
      (message) => {
        messages.push(message);
      },
      {
        claudeQueryFactory: queryFactory,
        operation: {
          commandType: "StartReview",
          payload: { target: { type: "uncommittedChanges" } },
        },
      },
    );

    await expect(run.result).resolves.toMatchObject({
      output: {
        operation: "review",
        supportMode: "emulated",
        text: "No actionable findings.",
      },
      providerResumeCursor: "session-review-warning",
    });
    expect(messages).toContainEqual({
      type: "event",
      eventType: "runtime.provider.warning",
      payload: {
        provider: "claudeAgent",
        message:
          "Claude Agent SDK marked a successful read-only Review with text as an error; Synara accepted the review because no explicit errors were reported.",
      },
    });
  });

  it("redacts Provider credentials from SDK terminal errors", async () => {
    const controlledProxy = "http://provider-user:provider-password@proxy.example.test:8080";
    const ambient = {
      SECRET: "ordinary-secret",
      HOST_SECRET: "host-secret",
      AWS_ACCESS_KEY_ID: "aws-key",
      AWS_SECRET_ACCESS_KEY: "aws-secret",
      GITHUB_TOKEN: "github-secret",
      DATABASE_URL: "postgres://user:secret@db/synara",
      PGPASSWORD: "postgres-secret",
      MINIO_ROOT_PASSWORD: "minio-secret",
      ANTHROPIC_API_KEY: "ambient-anthropic-secret",
      HTTP_PROXY: "http://ambient-user:ambient-secret@proxy.example.test",
      SYNARA_PROVIDER_HTTP_PROXY: controlledProxy,
    } as const;
    const environment = { ...process.env, ...ambient };
    const queryFactory: ClaudeQueryFactory = ({ options }) =>
      fakeQuery(
        (async function* () {
          const environment = requiredOptions(options).env;
          expect(environment?.PATH).toBeTruthy();
          expect(environment?.ANTHROPIC_API_KEY).toBe("provider-secret");
          expect(environment?.HTTP_PROXY).toBe(controlledProxy);
          expect(environment?.SYNARA_PROVIDER_HTTP_PROXY).toBeUndefined();
          for (const name of Object.keys(ambient).filter(
            (candidate) =>
              candidate !== "ANTHROPIC_API_KEY" &&
              candidate !== "HTTP_PROXY" &&
              candidate !== "SYNARA_PROVIDER_HTTP_PROXY",
          )) {
            expect(environment?.[name]).toBeUndefined();
          }
          yield sdkMessage({
            type: "result",
            subtype: "error_during_execution",
            is_error: true,
            errors: [`request failed via ${controlledProxy} with provider-secret`],
            usage: {},
            session_id: "session-error",
          });
        })(),
      );
    const run = startProviderHostRun(
      claudeInput({ inputText: "fail safely" }),
      { payload: { apiKey: "provider-secret" } },
      () => {},
      { claudeQueryFactory: queryFactory, environment },
    );

    await expect(run.result).rejects.toThrow("request failed via [REDACTED] with [REDACTED]");
    await expect(run.result).rejects.not.toThrow("provider-secret");
    await expect(run.result).rejects.not.toThrow(controlledProxy);
  });
});

function claudeInput(input: {
  inputText: string;
  runtimeMode?: "approval-required" | "full-access";
  interactionMode?: "default" | "plan";
  generation?: number;
  runtimeOutputDirectory?: string;
  workspaceDirectory?: string;
}): RunnerInput {
  return {
    execution: {
      id: "execution-claude-sdk",
      ...(input.generation === undefined ? {} : { generation: input.generation }),
    },
    workload: {
      provider: "claudeAgent",
      model: "claude-test",
      inputText: input.inputText,
      runtimeMode: input.runtimeMode ?? "full-access",
      interactionMode: input.interactionMode ?? "default",
    },
    workspaceDirectory: input.workspaceDirectory ?? "/tmp/synara-claude-runtime",
    ...(input.runtimeOutputDirectory
      ? { runtimeOutputDirectory: input.runtimeOutputDirectory }
      : {}),
  };
}

function nextInteraction(
  messages: RunnerMessage[],
  interactionType: "approval" | "user-input",
): Promise<Extract<RunnerMessage, { type: "interaction" }>> {
  return new Promise((resolve) => {
    const timer = setInterval(() => {
      const message = messages.find(
        (item): item is Extract<RunnerMessage, { type: "interaction" }> =>
          item.type === "interaction" && item.interactionType === interactionType,
      );
      if (!message) return;
      clearInterval(timer);
      resolve(message);
    }, 1);
  });
}

function nextInteractions(
  messages: RunnerMessage[],
  interactionType: "approval" | "user-input",
  count: number,
): Promise<ReadonlyArray<Extract<RunnerMessage, { type: "interaction" }>>> {
  return new Promise((resolve) => {
    const timer = setInterval(() => {
      const matching = messages.filter(
        (item): item is Extract<RunnerMessage, { type: "interaction" }> =>
          item.type === "interaction" && item.interactionType === interactionType,
      );
      if (matching.length < count) return;
      clearInterval(timer);
      resolve(matching.slice(0, count));
    }, 1);
  });
}

function fakeQuery(
  generator: AsyncGenerator<SDKMessage, void>,
  options: { onInterrupt?: () => void } = {},
): ClaudeQueryRuntime {
  return {
    [Symbol.asyncIterator]: () => generator,
    interrupt: async () => {
      options.onInterrupt?.();
    },
    close: () => {},
  };
}

async function promptText(prompt: string | AsyncIterable<SDKUserMessage>): Promise<string> {
  if (typeof prompt === "string") return prompt;
  const message = await prompt[Symbol.asyncIterator]().next();
  if (message.done) return "";
  const content = (message.value.message as { content?: unknown }).content;
  if (!Array.isArray(content)) return "";
  return content
    .flatMap((block) => {
      if (!block || typeof block !== "object") return [];
      const record = block as Record<string, unknown>;
      return record.type === "text" && typeof record.text === "string" ? [record.text] : [];
    })
    .join("");
}

function messageText(message: SDKUserMessage | undefined): string {
  const content = (message?.message as { content?: unknown } | undefined)?.content;
  if (!Array.isArray(content)) return "";
  return content
    .flatMap((block) => {
      if (!block || typeof block !== "object") return [];
      const record = block as Record<string, unknown>;
      return record.type === "text" && typeof record.text === "string" ? [record.text] : [];
    })
    .join("");
}

function requiredOptions(options: ClaudeQueryOptions | undefined): ClaudeQueryOptions {
  if (!options) throw new Error("Expected Claude query options");
  return options;
}

function systemInit(sessionId: string, model: string): Record<string, unknown> {
  return {
    type: "system",
    subtype: "init",
    session_id: sessionId,
    model,
  };
}

function successResult(
  sessionId: string,
  result: string,
  usage: Record<string, number>,
): Record<string, unknown> {
  return {
    type: "result",
    subtype: "success",
    is_error: false,
    result,
    usage,
    session_id: sessionId,
  };
}

function sdkMessage(value: Record<string, unknown>): SDKMessage {
  return value as unknown as SDKMessage;
}
