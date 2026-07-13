import type {
  Options as ClaudeQueryOptions,
  SDKMessage,
  SDKUserMessage,
} from "@anthropic-ai/claude-agent-sdk";
import { describe, expect, it } from "vitest";

import type {
  ClaudeQueryFactory,
  ClaudeQueryRuntime,
} from "./claudeAgentSdkRuntime";
import {
  startProviderHostRun,
  type RunnerInput,
  type RunnerMessage,
} from "./providerHost";

describe("Claude Agent SDK runtime", () => {
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
              title: "Run repository status",
            },
          );
          expect(decision).toMatchObject({ behavior: "allow" });
          yield sdkMessage({
            type: "user",
            session_id: "session-approval",
            message: {
              content: [
                { type: "tool_result", tool_use_id: "tool-approval", content: "clean" },
              ],
            },
          });
          yield sdkMessage({
            type: "assistant",
            session_id: "session-approval",
            message: { content: [{ type: "text", text: "approved" }] },
          });
          yield sdkMessage(successResult("session-approval", "approved", {
            input_tokens: 4,
            output_tokens: 2,
          }));
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
          payload: {
            provider: "claudeAgent",
            itemType: "Bash",
            status: "started",
            itemId: "tool-approval",
          },
        },
        {
          type: "event",
          eventType: "runtime.provider.activity",
          payload: {
            provider: "claudeAgent",
            itemType: "Bash",
            status: "completed",
            itemId: "tool-approval",
          },
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
            throw new Error("No conversation found with session ID session-invalid");
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
          "Native Claude resume was unavailable; rebuilt the turn from authoritative history.",
      },
    });
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
          yield sdkMessage(successResult("session-steer", "steered", {}));
        })(),
      );
    const run = startProviderHostRun(
      claudeInput({ inputText: "long task" }),
      null,
      () => {},
      { claudeQueryFactory: queryFactory },
    );

    await initialReadPromise;
    await run.steer?.({ inputText: "focus on the failing test" });
    await expect(run.result).resolves.toMatchObject({
      output: { text: "steered" },
      providerResumeCursor: "session-steer",
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
              content: [
                { type: "tool_use", id: "tool-pending", name: "Bash", input: {} },
              ],
            },
          });
          const decision = await requiredOptions(options).canUseTool?.(
            "Bash",
            { command: "sleep 30" },
            {
              signal: new AbortController().signal,
              toolUseID: "tool-pending",
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

  it("redacts Provider credentials from SDK terminal errors", async () => {
    const controlledProxy =
      "http://provider-user:provider-password@proxy.example.test:8080";
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

    await expect(run.result).rejects.toThrow(
      "request failed via [REDACTED] with [REDACTED]",
    );
    await expect(run.result).rejects.not.toThrow("provider-secret");
    await expect(run.result).rejects.not.toThrow(controlledProxy);
  });
});

function claudeInput(
  input: {
    inputText: string;
    runtimeMode?: "approval-required" | "full-access";
    interactionMode?: "default" | "plan";
  },
): RunnerInput {
  return {
    execution: { id: "execution-claude-sdk" },
    workload: {
      provider: "claudeAgent",
      model: "claude-test",
      inputText: input.inputText,
      runtimeMode: input.runtimeMode ?? "full-access",
      interactionMode: input.interactionMode ?? "default",
    },
    workspaceDirectory: "/tmp/synara-claude-runtime",
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

async function promptText(
  prompt: string | AsyncIterable<SDKUserMessage>,
): Promise<string> {
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
