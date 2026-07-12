import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import {
  createRedactor,
  normalizeClaudeEvent,
  providerEnvironment,
  reconstructedPrompt,
  startProviderHostRun,
} from "./providerHost";

describe("provider credential isolation", () => {
  it("maps Codex credentials and removes Worker secrets", () => {
    const result = providerEnvironment(
      {
        PATH: "/bin",
        SYNARA_WORKER_REGISTRATION_TOKEN: "worker-secret",
        SYNARA_AGENTD_ASSIGNED_EXECUTION_ID: "execution-id",
      },
      "codex",
      { payload: { apiKey: "provider-secret", baseUrl: "https://api.example.test" } },
    );
    expect(result.environment).toMatchObject({
      PATH: "/bin",
      OPENAI_API_KEY: "provider-secret",
      OPENAI_BASE_URL: "https://api.example.test",
    });
    expect(result.environment.SYNARA_WORKER_REGISTRATION_TOKEN).toBeUndefined();
    expect(result.environment.SYNARA_AGENTD_ASSIGNED_EXECUTION_ID).toBeUndefined();
    expect(result.redact("failed with provider-secret")).toBe("failed with [REDACTED]");
  });

  it("rejects generic environment injection", () => {
    expect(() =>
      providerEnvironment({}, "claudeAgent", {
        payload: { apiKey: "secret", environment: { MALICIOUS: "value" } },
      }),
    ).toThrow("unsupported fields");
  });
});

describe("provider event normalization", () => {
  it("normalizes Claude text and usage", () => {
    const state = { text: [] as string[] };
    const redact = createRedactor([]);
    normalizeClaudeEvent(
      { type: "system", subtype: "init", session_id: "session-1", model: "sonnet" },
      state,
      redact,
    );
    expect(
      normalizeClaudeEvent(
        {
          type: "assistant",
          message: { content: [{ type: "text", text: "hello" }] },
        },
        state,
        redact,
      ),
    ).toEqual([
      { type: "event", eventType: "runtime.output.delta", payload: { text: "hello" } },
    ]);
    expect(state).toMatchObject({ cursor: "session-1", model: "sonnet", text: ["hello"] });
  });
});

describe("durable conversation reconstruction", () => {
  it("separates prior transcript content from the current user turn", () => {
    const prompt = reconstructedPrompt({
      execution: { id: "execution-1" },
      workload: {
        provider: "codex",
        inputText: "current question",
        conversationHistory: [
          { role: "user", text: "prior question" },
          { role: "assistant", text: "prior answer" },
        ],
      },
      workspaceDirectory: "/tmp/workspace",
    });
    expect(prompt).toContain("<user>\nprior question\n</user>");
    expect(prompt).toContain("<assistant>\nprior answer\n</assistant>");
    expect(prompt).toContain("<current_user>\ncurrent question\n</current_user>");
  });
});

describe("provider process lifecycle", () => {
  it("terminates the active provider subprocess on interrupt", async () => {
    const directory = mkdtempSync(join(tmpdir(), "synara-provider-host-"));
    const executable = join(directory, "codex");
    writeFileSync(
      executable,
      "#!/bin/sh\n/bin/cat >/dev/null\nwhile :; do /bin/sleep 1; done\n",
      "utf8",
    );
    chmodSync(executable, 0o700);
    const previousPath = process.env.PATH;
    process.env.PATH = `${directory}:${previousPath ?? ""}`;
    try {
      const run = startProviderHostRun(
        {
          execution: { id: "execution-interrupt" },
          workload: { provider: "codex", inputText: "wait" },
          workspaceDirectory: directory,
        },
        null,
        () => {},
      );
      await new Promise((resolve) => setTimeout(resolve, 20));
      run.interrupt();
      await expect(run.result).rejects.toThrow("interrupted");
    } finally {
      process.env.PATH = previousPath;
      rmSync(directory, { recursive: true, force: true });
    }
  });
});
