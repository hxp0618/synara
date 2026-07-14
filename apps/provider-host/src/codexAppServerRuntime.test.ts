import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import {
  startProviderHostRun,
  type RunnerMessage,
} from "./providerHost";

const CONTROLLED_PROVIDER_PROXY =
  "http://provider-user:provider-password@proxy.example.test:8080";

describe("Codex app-server runtime", () => {
  it("delivers a native approval response and returns streamed output", async () => {
    await withFakeCodex("approval", async (directory, _tracePath, environment) => {
      const messages: RunnerMessage[] = [];
      const interaction = waitForInteraction(messages, (message) => message.interactionType === "approval");
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
      interactionType: "approval",
      expectedRequestId: "codex:generation-7:approval-rpc",
    },
    {
      scenario: "user-input",
      generation: 8,
      interactionType: "user-input",
      expectedRequestId: "codex:generation-8:user-input-rpc",
    },
  ] as const)(
    "scopes native $interactionType request IDs to Execution Generation $generation",
    async ({ scenario, generation, interactionType, expectedRequestId }) => {
      await withFakeCodex(scenario, async (directory, _tracePath, environment) => {
        const messages: RunnerMessage[] = [];
        const interaction = waitForInteraction(
          messages,
          (message) => message.interactionType === interactionType,
        );
        const run = startProviderHostRun(
          codexInput(directory, { generation, interactionMode: "plan" }),
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

  it("falls back to ResumeSnapshot reconstruction when native thread resume is invalid", async () => {
    await withFakeCodex("resume-rebuild", async (directory, _tracePath, environment) => {
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
            },
          },
        },
        null,
        () => {},
        { environment },
      );

      await expect(run.result).resolves.toMatchObject({
        output: { text: "rebuilt" },
        providerResumeCursor: "thread-new",
      });
    });
  });

  it("redacts an authenticated controlled proxy from Provider output", async () => {
    await withFakeCodex("proxy-output", async (directory, _tracePath, environment) => {
      const run = startProviderHostRun(codexInput(directory), null, () => {}, { environment });
      const result = await run.result;

      expect(result).toMatchObject({
        output: { provider: "codex", text: "[REDACTED]" },
      });
      expect(JSON.stringify(result)).not.toContain(CONTROLLED_PROVIDER_PROXY);
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
  modes: { interactionMode?: "default" | "plan"; generation?: number } = {},
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
    | "resume-rebuild"
    | "interrupt"
    | "steer"
    | "proxy-output",
  run: (
    directory: string,
    tracePath: string,
    environment: NodeJS.ProcessEnv,
  ) => Promise<void>,
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
    | "resume-rebuild"
    | "interrupt"
    | "steer"
    | "proxy-output",
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
  if (process.env[name] !== undefined) {
    process.stderr.write("ambient secret leaked to Codex child: " + name + "\\n");
    process.exit(92);
  }
}
const send = (message) => process.stdout.write(JSON.stringify(message) + "\\n");
const longApprovalId = "approval-" + "x".repeat(400);
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
    if (scenario === "resume-rebuild") send({ id: message.id, error: { code: -2, message: "missing thread" } });
    else send({ id: message.id, result: { thread: { id: message.params.threadId } } });
  } else if (message.method === "thread/start") {
    if (scenario === "resume") send({ id: message.id, error: { code: -1, message: "unexpected thread/start" } });
    else send({ id: message.id, result: { thread: { id: "thread-new" } } });
  } else if (message.method === "turn/start") {
    if (scenario === "resume-rebuild") {
      const prompt = message.params?.input?.[0]?.text ?? "";
			if (!prompt.includes("<synara_resume_snapshot_json>") || !prompt.includes("Focused tests passed") || !prompt.includes("<current_user>\\ncontinue\\n</current_user>")) process.exit(3);
    }
    send({ id: message.id, result: { turn: { id: "turn-1", items: [], status: "inProgress", error: null } } });
    if (scenario === "approval") {
      send({ id: "approval-rpc", method: "item/commandExecution/requestApproval", params: { threadId: "thread-new", turnId: "turn-1", itemId: "command-1", command: "git status --short", cwd: process.cwd(), reason: "Run a status check" } });
    } else if (scenario === "long-approval") {
      send({ id: longApprovalId, method: "item/commandExecution/requestApproval", params: { threadId: "thread-new", turnId: "turn-1", itemId: "command-long", command: "git status --short", cwd: process.cwd(), reason: "Run a status check" } });
    } else if (scenario === "user-input") {
      send({ id: "user-input-rpc", method: "item/tool/requestUserInput", params: { threadId: "thread-new", turnId: "turn-1", itemId: "input-1", autoResolutionMs: null, questions: [{ id: "environment", header: "Environment", question: "Where should this run?", isOther: false, isSecret: false, options: [{ label: "Staging", description: "Use staging." }] }] } });
    } else if (scenario === "resume") {
      complete("resumed");
    } else if (scenario === "resume-rebuild") {
      complete("rebuilt");
    } else if (scenario === "interrupt") {
      send({ method: "item/agentMessage/delta", params: { threadId: "thread-new", turnId: "turn-1", itemId: "agent-1", delta: "waiting" } });
    } else if (scenario === "steer") {
      send({ method: "item/agentMessage/delta", params: { threadId: "thread-new", turnId: "turn-1", itemId: "agent-1", delta: "working" } });
    } else if (scenario === "proxy-output") {
      complete(process.env.HTTP_PROXY ?? "missing-proxy");
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
  } else if (message.id === longApprovalId) {
    if (message.result?.decision !== "accept") process.exit(2);
    complete("approved");
  } else if (message.id === "user-input-rpc") {
    complete(message.result?.answers?.environment?.answers?.[0] ?? "missing");
  }
});
`;
}
