import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import {
  startProviderHostRun,
  type RunnerMessage,
} from "./providerHost";

describe("Codex app-server runtime", () => {
  it("delivers a native approval response and returns streamed output", async () => {
    await withFakeCodex("approval", async (directory) => {
      const messages: RunnerMessage[] = [];
      const interaction = waitForInteraction(messages, (message) => message.interactionType === "approval");
      const run = startProviderHostRun(codexInput(directory), null, (message) => messages.push(message));

      const request = await interaction;
      expect(request.payload).toMatchObject({
        requestId: "codex:approval-rpc",
        requestKind: "command",
        command: "git status --short",
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
        },
      });
    });
  });

  it("answers native structured user input", async () => {
    await withFakeCodex("user-input", async (directory) => {
      const messages: RunnerMessage[] = [];
      const interaction = waitForInteraction(
        messages,
        (message) => message.interactionType === "user-input",
      );
      const run = startProviderHostRun(
        codexInput(directory, { interactionMode: "plan" }),
        null,
        (message) => messages.push(message),
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

  it("uses thread/resume even when authoritative history is available", async () => {
    await withFakeCodex("resume", async (directory) => {
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
      );

      await expect(run.result).resolves.toMatchObject({
        output: { text: "resumed" },
        providerResumeCursor: "thread-resume",
      });
    });
  });

  it("sends native turn/interrupt before terminating the app-server", async () => {
    await withFakeCodex("interrupt", async (directory, tracePath) => {
      let started!: () => void;
      const startedPromise = new Promise<void>((resolve) => {
        started = resolve;
      });
      const run = startProviderHostRun(codexInput(directory), null, (message) => {
        if (message.type === "event" && message.eventType === "runtime.output.delta") started();
      });

      await startedPromise;
      run.interrupt();
      await expect(run.result).rejects.toThrow("interrupted");
      expect(readFileSync(tracePath, "utf8")).toContain("turn/interrupt");
    });
  });
});

function codexInput(
  directory: string,
  modes: { interactionMode?: "default" | "plan" } = {},
) {
  return {
    execution: { id: "execution-codex-app-server" },
    workload: {
      provider: "codex",
      model: "gpt-test",
      inputText: "do work",
      runtimeMode: "approval-required" as const,
      interactionMode: modes.interactionMode ?? "default",
    },
    workspaceDirectory: directory,
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
  scenario: "approval" | "user-input" | "resume" | "interrupt",
  run: (directory: string, tracePath: string) => Promise<void>,
): Promise<void> {
  const directory = mkdtempSync(join(tmpdir(), "synara-codex-app-server-"));
  const executable = join(directory, "codex");
  const tracePath = join(directory, "trace.log");
  writeFileSync(executable, fakeCodexSource(), "utf8");
  chmodSync(executable, 0o700);
  const previousPath = process.env.PATH;
  const previousScenario = process.env.FAKE_CODEX_SCENARIO;
  const previousTrace = process.env.FAKE_CODEX_TRACE;
  process.env.PATH = `${directory}:${previousPath ?? ""}`;
  process.env.FAKE_CODEX_SCENARIO = scenario;
  process.env.FAKE_CODEX_TRACE = tracePath;
  try {
    await run(directory, tracePath);
  } finally {
    process.env.PATH = previousPath;
    if (previousScenario === undefined) delete process.env.FAKE_CODEX_SCENARIO;
    else process.env.FAKE_CODEX_SCENARIO = previousScenario;
    if (previousTrace === undefined) delete process.env.FAKE_CODEX_TRACE;
    else process.env.FAKE_CODEX_TRACE = previousTrace;
    rmSync(directory, { recursive: true, force: true });
  }
}

function fakeCodexSource(): string {
  return `#!/usr/bin/env node
const fs = require("node:fs");
const readline = require("node:readline");
const scenario = process.env.FAKE_CODEX_SCENARIO;
const trace = process.env.FAKE_CODEX_TRACE;
const send = (message) => process.stdout.write(JSON.stringify(message) + "\\n");
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
  } else if (message.method === "thread/resume") {
    send({ id: message.id, result: { thread: { id: message.params.threadId } } });
  } else if (message.method === "thread/start") {
    if (scenario === "resume") send({ id: message.id, error: { code: -1, message: "unexpected thread/start" } });
    else send({ id: message.id, result: { thread: { id: "thread-new" } } });
  } else if (message.method === "turn/start") {
    send({ id: message.id, result: { turn: { id: "turn-1", items: [], status: "inProgress", error: null } } });
    if (scenario === "approval") {
      send({ id: "approval-rpc", method: "item/commandExecution/requestApproval", params: { threadId: "thread-new", turnId: "turn-1", itemId: "command-1", command: "git status --short", cwd: process.cwd(), reason: "Run a status check" } });
    } else if (scenario === "user-input") {
      send({ id: "user-input-rpc", method: "item/tool/requestUserInput", params: { threadId: "thread-new", turnId: "turn-1", itemId: "input-1", autoResolutionMs: null, questions: [{ id: "environment", header: "Environment", question: "Where should this run?", isOther: false, isSecret: false, options: [{ label: "Staging", description: "Use staging." }] }] } });
    } else if (scenario === "resume") {
      complete("resumed");
    } else if (scenario === "interrupt") {
      send({ method: "item/agentMessage/delta", params: { threadId: "thread-new", turnId: "turn-1", itemId: "agent-1", delta: "waiting" } });
    }
  } else if (message.method === "turn/interrupt") {
    send({ id: message.id, result: {} });
    send({ method: "turn/completed", params: { threadId: "thread-new", turn: { id: "turn-1", items: [], status: "interrupted", error: null } } });
  } else if (message.id === "approval-rpc") {
    if (message.result?.decision !== "accept") process.exit(2);
    send({ method: "thread/tokenUsage/updated", params: { threadId: "thread-new", turnId: "turn-1", tokenUsage: { total: {}, last: { totalTokens: 8, inputTokens: 5, cachedInputTokens: 1, outputTokens: 3, reasoningOutputTokens: 0 }, modelContextWindow: 100 } } });
    complete("approved");
  } else if (message.id === "user-input-rpc") {
    complete(message.result?.answers?.environment?.answers?.[0] ?? "missing");
  }
});
`;
}
