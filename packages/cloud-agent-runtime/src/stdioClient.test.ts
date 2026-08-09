import { closeSync, mkdtempSync, openSync, rmSync, writeFileSync } from "node:fs";
import type { ChildProcessWithoutNullStreams } from "node:child_process";
import { EventEmitter } from "node:events";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { PassThrough } from "node:stream";

import { describe, expect, it, vi } from "vitest";

import type { CloudAgentCommandEnvelope } from "@synara/cloud-agent-protocol";

import { createCloudAgentStdioClient } from "./stdioClient";

const RESPONDING_RUNTIME = String.raw`
const readline = require("node:readline");
const lines = readline.createInterface({ input: process.stdin });
lines.on("line", (line) => {
  const command = JSON.parse(line);
  const base = {
    requestId: command.requestId,
    protocolVersion: command.protocolVersion,
    executionId: command.executionId,
    generation: command.generation,
    commandId: command.commandId,
    occurredAt: new Date().toISOString(),
  };
  process.stdout.write(JSON.stringify({
    ...base,
    messageType: "Progress",
    payload: { phase: "started" },
  }) + "\n");
  process.stdout.write(JSON.stringify({
    ...base,
    messageType: "Result",
    payload: { echoed: command.payload },
  }) + "\n");
});
`;

const DELAYED_RUNTIME = String.raw`
const readline = require("node:readline");
const lines = readline.createInterface({ input: process.stdin });
lines.on("line", (line) => {
  const command = JSON.parse(line);
  setTimeout(() => process.stdout.write(JSON.stringify({
    requestId: command.requestId,
    protocolVersion: command.protocolVersion,
    executionId: command.executionId,
    generation: command.generation,
    commandId: command.commandId,
    occurredAt: new Date().toISOString(),
    messageType: "Result",
    payload: {},
  }) + "\n"), 25);
});
`;

const CREDENTIAL_RUNTIME = String.raw`
const { readFileSync } = require("node:fs");
const readline = require("node:readline");
const lines = readline.createInterface({ input: process.stdin });
lines.once("line", (line) => {
  const command = JSON.parse(line);
  const credentialFd = Number(process.env.SYNARA_PROVIDER_CREDENTIAL_FD);
  process.stdout.write(JSON.stringify({
    requestId: command.requestId,
    protocolVersion: command.protocolVersion,
    executionId: command.executionId,
    generation: command.generation,
    commandId: command.commandId,
    occurredAt: new Date().toISOString(),
    messageType: "Result",
    payload: {
      credential: readFileSync(credentialFd, "utf8"),
      credentialFd,
    },
  }) + "\n");
});
`;

const MISMATCHED_RUNTIME = RESPONDING_RUNTIME.replace(
  "executionId: command.executionId,",
  'executionId: "wrong-execution",',
);

const SIGTERM_IGNORING_MALFORMED_RUNTIME = String.raw`
process.on("SIGTERM", () => {});
setInterval(() => {}, 1_000);
process.stdin.once("data", () => process.stdout.write("not-json\n"));
`;

const command = (commandId: string): CloudAgentCommandEnvelope => ({
  requestId: `request-${commandId}`,
  protocolVersion: { major: 2, minor: 2 },
  executionId: "execution-1",
  generation: 1,
  commandType: "Describe",
  commandId,
  occurredAt: "2026-08-09T00:00:00.000Z",
  payload: { provider: "codex" },
});

describe("createCloudAgentStdioClient", () => {
  it("multiplexes events and terminal results by command id", async () => {
    const client = createCloudAgentStdioClient({
      command: process.execPath,
      args: ["-e", RESPONDING_RUNTIME, "--"],
    });
    const messages: string[] = [];
    const unsubscribe = client.subscribe((message) => messages.push(message.messageType));

    const [first, second] = await Promise.all([
      client.execute(command("command-1")),
      client.execute(command("command-2")),
    ]);

    expect(first.messageType).toBe("Result");
    expect(second.messageType).toBe("Result");
    expect(messages).toEqual(["Progress", "Result", "Progress", "Result"]);
    unsubscribe();
    await client.close();
  });

  it("rejects duplicate in-flight command ids", async () => {
    const client = createCloudAgentStdioClient({
      command: process.execPath,
      args: ["-e", DELAYED_RUNTIME, "--"],
    });
    const first = client.execute(command("same-command"));
    await expect(client.execute(command("same-command"))).rejects.toThrow("already in flight");
    await first;
    await client.close();
  });

  it("fails closed when the runtime emits malformed JSON", async () => {
    const client = createCloudAgentStdioClient({
      command: process.execPath,
      args: ["-e", 'process.stdin.once("data", () => process.stdout.write("not-json\\n"));', "--"],
    });
    await expect(client.execute(command("invalid-json"))).rejects.toThrow("invalid JSON");
    await client.close();
  });

  it("fails closed when the runtime emits invalid UTF-8", async () => {
    const client = createCloudAgentStdioClient({
      command: process.execPath,
      args: [
        "-e",
        'process.stdin.once("data", () => process.stdout.write(Buffer.from([0xc3, 0x28, 0x0a])));',
        "--",
      ],
    });
    await expect(client.execute(command("invalid-utf8"))).rejects.toThrow("invalid UTF-8");
    await client.close();
  });

  it("shares protocol-fatal teardown with close and SIGKILLs a child that ignores SIGTERM", async () => {
    const client = createCloudAgentStdioClient({
      command: process.execPath,
      args: ["-e", SIGTERM_IGNORING_MALFORMED_RUNTIME, "--"],
      gracefulStopTimeoutMs: 25,
    });
    const pid = client.pid;
    expect(pid).toBeTypeOf("number");

    await expect(client.execute(command("fatal-reap"))).rejects.toThrow("invalid JSON");
    await Promise.all([client.close(), client.close()]);

    expect(processIsAlive(pid)).toBe(false);
  });

  it("rejects close within a bounded deadline when forced termination never exits", async () => {
    const signals: NodeJS.Signals[] = [];
    const child = new EventEmitter() as ChildProcessWithoutNullStreams;
    Object.assign(child, {
      stdin: new PassThrough(),
      stdout: new PassThrough(),
      stderr: new PassThrough(),
      pid: 99_999,
      exitCode: null,
      signalCode: null,
      kill(signal: NodeJS.Signals) {
        signals.push(signal);
        return true;
      },
    });
    const client = createCloudAgentStdioClient({
      command: "fake-cloud-agent-runtime",
      gracefulStopTimeoutMs: 5,
      spawnProcess: (() => child) as never,
    });

    await expect(client.close()).rejects.toThrow("did not exit within 5ms");
    expect(signals).toEqual(["SIGTERM", "SIGKILL"]);
  });

  it("fails closed when the runtime response has the wrong execution identity", async () => {
    const client = createCloudAgentStdioClient({
      command: process.execPath,
      args: ["-e", MISMATCHED_RUNTIME, "--"],
    });
    await expect(client.execute(command("wrong-execution"))).rejects.toThrow("execution identity");
    await client.close();
  });

  it("drops expected late frames after abort without killing the shared Runtime", async () => {
    const client = createCloudAgentStdioClient({
      command: process.execPath,
      args: ["-e", DELAYED_RUNTIME, "--"],
    });
    const abort = new AbortController();
    const first = client.execute(command("aborted-command"), abort.signal);
    abort.abort("test cancellation");
    await expect(first).rejects.toThrow("aborted");

    await new Promise((resolve) => setTimeout(resolve, 40));
    await expect(client.execute(command("next-command"))).resolves.toMatchObject({
      messageType: "Result",
      commandId: "next-command",
    });
    await client.close();
  });

  it("expires aborted tombstones, releases capacity, and rejects a later frame fail-closed", async () => {
    vi.useFakeTimers();
    try {
      const child = new EventEmitter() as ChildProcessWithoutNullStreams;
      const stdin = new PassThrough();
      const stdout = new PassThrough();
      const stderr = new PassThrough();
      Object.assign(child, {
        stdin,
        stdout,
        stderr,
        pid: 99_998,
        exitCode: null,
        signalCode: null,
        kill: () => true,
      });
      let firstCommand: CloudAgentCommandEnvelope | undefined;
      let inputBuffer = "";
      stdin.setEncoding("utf8");
      stdin.on("data", (chunk: string) => {
        inputBuffer += chunk;
        while (inputBuffer.includes("\n")) {
          const newline = inputBuffer.indexOf("\n");
          const line = inputBuffer.slice(0, newline);
          inputBuffer = inputBuffer.slice(newline + 1);
          const received = JSON.parse(line) as CloudAgentCommandEnvelope;
          firstCommand ??= received;
          if (received.commandId !== "after-expiry" || !firstCommand) continue;
          stdout.write(`${JSON.stringify(terminalFor(firstCommand))}\n`);
          queueMicrotask(() => {
            Object.assign(child, { exitCode: 1 });
            child.emit("exit", 1, null);
          });
        }
      });
      const client = createCloudAgentStdioClient({
        command: "fake-cloud-agent-runtime",
        spawnProcess: (() => child) as never,
      });

      for (let index = 0; index < 128; index += 1) {
        const controller = new AbortController();
        const execution = client.execute(command(`aborted-${index}`), controller.signal);
        controller.abort();
        await expect(execution).rejects.toThrow("aborted");
      }
      await expect(client.execute(command("at-capacity"))).rejects.toThrow(
        "128 commands in flight",
      );

      await vi.advanceTimersByTimeAsync(30_001);
      await expect(client.execute(command("after-expiry"))).rejects.toThrow("unknown command");
      await client.close();
    } finally {
      vi.useRealTimers();
    }
  });

  it("maps a caller-owned credential descriptor to child fd 3", async () => {
    const directory = mkdtempSync(join(tmpdir(), "cloud-agent-credential-"));
    const credentialPath = join(directory, "credential.json");
    writeFileSync(credentialPath, '{"token":"opaque-test-value"}');
    const credentialFd = openSync(credentialPath, "r");
    const client = createCloudAgentStdioClient({
      command: process.execPath,
      args: ["-e", CREDENTIAL_RUNTIME, "--"],
      credentialFd,
    });

    try {
      const result = await client.execute(command("credential-command"));
      expect(result).toMatchObject({
        messageType: "Result",
        payload: {
          credential: '{"token":"opaque-test-value"}',
          credentialFd: 3,
        },
      });
    } finally {
      await client.close();
      closeSync(credentialFd);
      rmSync(directory, { recursive: true });
    }
  });

  it("rejects an environment override that conflicts with credentialFd mapping", () => {
    expect(() =>
      createCloudAgentStdioClient({
        command: process.execPath,
        credentialFd: 7,
        environment: { SYNARA_PROVIDER_CREDENTIAL_FD: "7" },
      }),
    ).toThrow("conflicting SYNARA_PROVIDER_CREDENTIAL_FD override");
  });
});

function processIsAlive(pid: number | undefined): boolean {
  if (pid === undefined) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

function terminalFor(input: CloudAgentCommandEnvelope) {
  return {
    requestId: input.requestId,
    protocolVersion: input.protocolVersion,
    executionId: input.executionId,
    generation: input.generation,
    commandId: input.commandId,
    occurredAt: input.occurredAt,
    messageType: "Result",
    payload: {},
  };
}
