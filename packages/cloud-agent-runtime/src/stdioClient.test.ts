import { closeSync, mkdtempSync, openSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

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
