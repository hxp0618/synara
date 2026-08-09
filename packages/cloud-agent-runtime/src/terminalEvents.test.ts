import { mkdirSync, mkdtempSync, rmSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import type { RunnerMessage } from "./providerHost";
import { createRedactor } from "./providerHost";
import {
  createTerminalOutputStream,
  emitTerminalOutput,
  terminalCommandSummary,
  terminalCwdLabel,
} from "./terminalEvents";

describe("terminal event helpers", () => {
  it("redacts command summaries before applying their bound", () => {
    expect(
      terminalCommandSummary(["curl", "-H", "Authorization: secret-token"], (value) =>
        value.replace("secret-token", "[redacted]"),
      ),
    ).toBe("curl -H Authorization: [REDACTED]");
  });

  it("keeps command summaries single-line and structurally hides common credentials", () => {
    expect(
      terminalCommandSummary(
        "TOKEN=unregistered curl --password hunter2 https://user:pass@example.test\nprivate body",
        (value) => value,
      ),
    ).toBe("TOKEN=[REDACTED] curl --password [REDACTED] https://user:[REDACTED]@example.test");
  });

  it("labels cwd relative to the canonical workspace without exposing outside parents", () => {
    const directory = mkdtempSync(join(tmpdir(), "synara-terminal-paths-"));
    const workspace = join(directory, "workspace");
    const workspaceAlias = join(directory, "workspace-alias");
    const nested = join(workspace, "apps", "provider-host");
    const outside = join(directory, "sensitive-parent", "checkout");
    mkdirSync(nested, { recursive: true });
    mkdirSync(outside, { recursive: true });
    symlinkSync(workspace, workspaceAlias, "dir");

    try {
      expect(terminalCwdLabel(workspaceAlias, workspace)).toBe(".");
      expect(terminalCwdLabel(workspaceAlias, "apps/provider-host")).toBe("apps/provider-host");
      expect(terminalCwdLabel(workspaceAlias, outside)).toBe("checkout");
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  it("redacts the full output before splitting it into UTF-8-safe 8 KiB chunks", () => {
    const messages: RunnerMessage[] = [];
    const secret = "cross-boundary-secret";
    const output = `${"a".repeat(8_180)}${secret}${"界".repeat(4_000)}`;

    emitTerminalOutput({
      emit: (message) => messages.push(message),
      provider: "codex",
      terminalId: "terminal-1",
      output,
      redact: (value) => value.replace(secret, "[redacted]"),
    });

    const chunks = messages.map((message) => {
      expect(message).toMatchObject({
        type: "event",
        eventType: "runtime.command.output",
        payload: {
          provider: "codex",
          terminalId: "terminal-1",
          encoding: "utf-8",
        },
      });
      if (message.type !== "event") throw new Error("Expected a runtime event.");
      return String(message.payload.text);
    });

    expect(chunks.length).toBeGreaterThan(1);
    expect(chunks.every((chunk) => Buffer.byteLength(chunk, "utf8") <= 8 * 1024)).toBe(true);
    expect(chunks.join("")).toBe(output.replace(secret, "[redacted]"));
    expect(chunks.join("")).not.toContain(secret);
    expect(chunks.every((chunk) => !chunk.includes("�"))).toBe(true);
  });

  it("holds a bounded tail so secrets split across provider deltas are never emitted", () => {
    const messages: RunnerMessage[] = [];
    const stream = createTerminalOutputStream({
      emit: (message) => messages.push(message),
      provider: "codex",
      terminalId: "terminal-split-secret",
      redact: createRedactor(["provider-secret"]),
    });

    stream.write("before provider-");
    stream.write("secret after");
    stream.flush();

    const output = messages
      .filter(
        (message): message is Extract<RunnerMessage, { type: "event" }> => message.type === "event",
      )
      .map((message) => String(message.payload.text))
      .join("");
    expect(output).toBe("before [REDACTED] after");
    expect(output).not.toContain("provider-secret");
  });

  it("never splits a surrogate pair at the streaming redaction boundary", () => {
    const messages: RunnerMessage[] = [];
    const stream = createTerminalOutputStream({
      emit: (message) => messages.push(message),
      provider: "codex",
      terminalId: "terminal-emoji",
      redact: createRedactor(["12345"]),
    });

    stream.write("ab😀xyz");
    stream.flush();

    const output = messages
      .filter(
        (message): message is Extract<RunnerMessage, { type: "event" }> => message.type === "event",
      )
      .map((message) => String(message.payload.text))
      .join("");
    expect(output).toBe("ab😀xyz");
    expect(output).not.toContain("�");
  });
});
