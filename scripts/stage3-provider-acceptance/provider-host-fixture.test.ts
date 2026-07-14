import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

import {
  PROVIDER_CAPABILITY_CATALOG,
  PROVIDER_HOST_MAX_MESSAGE_BYTES,
  PROVIDER_HOST_PROTOCOL_VERSION,
  PROVIDER_HOST_PROVIDER_KINDS,
  ProviderHostDescriptor,
  ProviderHostMessageEnvelope,
  type ProviderHostCommandEnvelope,
  type ProviderHostMessageEnvelope as ProviderHostMessage,
} from "@synara/contracts/provider-host";
import { Schema } from "effect";
import { afterEach, describe, expect, it } from "vitest";

import {
  Stage3ProviderAcceptanceHost,
  STAGE3_FIXTURE_CREDENTIAL_SENTINEL,
  fixtureDescriptor,
  parseFixtureScenarios,
} from "./provider-host-fixture";

const decodeDescriptor = Schema.decodeUnknownSync(ProviderHostDescriptor);
const decodeMessage = Schema.decodeUnknownSync(ProviderHostMessageEnvelope);
const temporaryDirectories: string[] = [];

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    rmSync(directory, { recursive: true, force: true });
  }
});

describe("Stage 3 Provider Host acceptance fixture", () => {
  it("describes the current Protocol 2.1 ordered 8 Provider by 28 Capability catalog", () => {
    const enabled = new Set(["codex", "claudeAgent"] as const);

    for (const provider of PROVIDER_HOST_PROVIDER_KINDS) {
      const descriptor = decodeDescriptor(fixtureDescriptor(provider, enabled));
      const catalog = PROVIDER_CAPABILITY_CATALOG.providers.find(
        (entry) => entry.provider === provider,
      );

      expect(descriptor.protocolVersion).toEqual({ major: 2, minor: 1 });
      expect(descriptor.capabilityDescriptor.provider).toBe(provider);
      expect(descriptor.capabilityDescriptor.supportTier).toBe(catalog?.supportTier);
      expect(descriptor.capabilityDescriptor.capabilities).toEqual(catalog?.capabilities);
      expect(descriptor.capabilityDescriptor.releasePolicy.enabled).toBe(true);
      expect(descriptor.capabilityDescriptor.runtime.compatible).toBe(true);
      expect(descriptor.credentialDeliveryModes).toEqual(
        provider === "codex" || provider === "claudeAgent" ? ["anonymous-fd"] : [],
      );
    }
  });

  it("emits deterministic text, tool, usage, and materialized artifact messages before one Result", () => {
    const workspace = mkdtempSync(join(tmpdir(), "synara-stage3-fixture-"));
    temporaryDirectories.push(workspace);
    const output: ProviderHostMessage[] = [];
    const host = fixtureHost(output);

    host.handleCommand(
      command("StartSession", "session-1", {
        runnerInput: {
          workload: { provider: "codex" },
          workspaceDirectory: workspace,
        },
        runtimeEventVersion: 2,
      }),
    );
    host.handleCommand(
      command("SendTurn", "send-1", {
        inputText: "[text] [tool] [usage] [artifact]",
        runtimeEventVersion: 2,
      }),
    );

    const sendMessages = output.filter((message) => message.commandId === "send-1");
    expect(sendMessages.map((message) => message.messageType)).toEqual([
      "Event",
      "Event",
      "Event",
      "Event",
      "ArtifactCandidate",
      "Result",
    ]);
    expect(sendMessages.filter(isTerminal)).toHaveLength(1);
    expect(eventTypes(sendMessages)).toEqual([
      "content.delta",
      "item.started",
      "item.completed",
      "thread.token-usage.updated",
    ]);
    expect(
      sendMessages.find((message) => message.messageType === "ArtifactCandidate"),
    ).toMatchObject({
      payload: { artifact: { kind: "generated_file" } },
    });
    expect(readFileSync(join(workspace, ".synara-stage3-acceptance/artifact.txt"), "utf8")).toBe(
      "deterministic stage 3 acceptance artifact\n",
    );
    for (const message of output) expect(() => decodeMessage(message)).not.toThrow();
  });

  it("verifies the exact artifact sentinel from the persisted Workspace", () => {
    const workspace = mkdtempSync(join(tmpdir(), "synara-stage3-fixture-workspace-"));
    temporaryDirectories.push(workspace);
    const output: ProviderHostMessage[] = [];
    const host = fixtureHost(output);
    startCodexSession(host, workspace);

    host.handleCommand(command("SendTurn", "workspace-write", { inputText: "[artifact]" }));
    host.handleCommand(
      command("SendTurn", "workspace-verify", { inputText: "[workspace-verify]" }),
    );

    expect(messagesFor(output, "workspace-verify")).toMatchObject([
      {
        messageType: "Result",
        payload: {
          output: {
            workspaceEvidence: {
              artifactRelativePath: ".synara-stage3-acceptance/artifact.txt",
              artifactContentVerified: true,
            },
          },
        },
      },
    ]);

    writeFileSync(join(workspace, ".synara-stage3-acceptance/artifact.txt"), "tampered\n");
    host.handleCommand(
      command("SendTurn", "workspace-tampered", { inputText: "[workspace-verify]" }),
    );
    expect(messagesFor(output, "workspace-tampered")).toMatchObject([
      { messageType: "Error", error: { code: "workspace_invalid" } },
    ]);
  });

  it("fails closed when the artifact directory is a symbolic link outside the Workspace", () => {
    const workspace = mkdtempSync(join(tmpdir(), "synara-stage3-fixture-workspace-"));
    const outside = mkdtempSync(join(tmpdir(), "synara-stage3-fixture-outside-"));
    temporaryDirectories.push(workspace, outside);
    symlinkSync(outside, join(workspace, ".synara-stage3-acceptance"), "dir");
    const output: ProviderHostMessage[] = [];
    const host = fixtureHost(output);
    startCodexSession(host, workspace);

    host.handleCommand(
      command("SendTurn", "artifact-directory-symlink", { inputText: "[artifact]" }),
    );

    expect(messagesFor(output, "artifact-directory-symlink")).toMatchObject([
      { messageType: "Error", error: { code: "workspace_invalid" } },
    ]);
    expect(existsSync(join(outside, "artifact.txt"))).toBe(false);
  });

  it("fails closed without overwriting an artifact target symbolic link", () => {
    const workspace = mkdtempSync(join(tmpdir(), "synara-stage3-fixture-workspace-"));
    const outside = mkdtempSync(join(tmpdir(), "synara-stage3-fixture-outside-"));
    temporaryDirectories.push(workspace, outside);
    const artifactDirectory = join(workspace, ".synara-stage3-acceptance");
    const outsideArtifact = join(outside, "artifact.txt");
    mkdirSync(artifactDirectory);
    writeFileSync(outsideArtifact, "outside sentinel\n");
    symlinkSync(outsideArtifact, join(artifactDirectory, "artifact.txt"), "file");
    const output: ProviderHostMessage[] = [];
    const host = fixtureHost(output);
    startCodexSession(host, workspace);

    host.handleCommand(command("SendTurn", "artifact-target-symlink", { inputText: "[artifact]" }));

    expect(messagesFor(output, "artifact-target-symlink")).toMatchObject([
      { messageType: "Error", error: { code: "workspace_invalid" } },
    ]);
    expect(readFileSync(outsideArtifact, "utf8")).toBe("outside sentinel\n");
  });

  it("keeps approval and user-input turns active until their correlated resolution", () => {
    const output: ProviderHostMessage[] = [];
    const host = fixtureHost(output);
    startCodexSession(host);

    host.handleCommand(command("SendTurn", "send-approval", { inputText: "[approval]" }));
    expect(messagesFor(output, "send-approval").map((message) => message.messageType)).toEqual([
      "InteractionRequest",
    ]);
    host.handleCommand(
      command("ResolveApproval", "resolve-approval", {
        requestId: "fixture-approval-generation-1-1",
        resolution: { decision: "accept" },
      }),
    );
    expect(messagesFor(output, "resolve-approval").filter(isTerminal)).toHaveLength(1);
    expect(messagesFor(output, "send-approval").filter(isTerminal)).toHaveLength(1);

    host.handleCommand(command("SendTurn", "send-input", { inputText: "[user-input]" }));
    const interaction = messagesFor(output, "send-input")[0];
    expect(interaction).toMatchObject({
      messageType: "InteractionRequest",
      payload: {
        interactionType: "user-input",
        requestId: "fixture-user-input-generation-1-2",
      },
    });
    host.handleCommand(
      command("ResolveUserInput", "resolve-input", {
        requestId: "fixture-user-input-generation-1-2",
        resolution: { answers: { "fixture-choice": "Continue" } },
      }),
    );
    expect(messagesFor(output, "resolve-input").filter(isTerminal)).toHaveLength(1);
    expect(messagesFor(output, "send-input").filter(isTerminal)).toHaveLength(1);
  });

  it("supports steer and interrupt while preserving one terminal per command", () => {
    const output: ProviderHostMessage[] = [];
    const host = fixtureHost(output);
    startCodexSession(host);

    host.handleCommand(command("SendTurn", "send-steer", { inputText: "[steer]" }));
    host.handleCommand(
      command("SteerTurn", "steer-1", {
        targetCommandId: "send-steer",
        inputText: "focus on acceptance",
      }),
    );
    expect(eventTypes(messagesFor(output, "send-steer"))).toContain("content.delta");
    expect(messagesFor(output, "send-steer").filter(isTerminal)).toHaveLength(1);
    expect(messagesFor(output, "steer-1").filter(isTerminal)).toHaveLength(1);

    host.handleCommand(command("SendTurn", "send-interrupt", { inputText: "[approval]" }));
    host.handleCommand(
      command("InterruptTurn", "interrupt-1", { targetCommandId: "send-interrupt" }),
    );
    expect(messagesFor(output, "interrupt-1").at(-1)).toMatchObject({ messageType: "Result" });
    expect(messagesFor(output, "send-interrupt").at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "interrupted" },
    });
  });

  it.each(["approval", "user-input"] as const)(
    "changes deterministic %s IDs across recovered Execution Generations",
    (interactionType) => {
      const firstOutput: ProviderHostMessage[] = [];
      const firstHost = fixtureHost(firstOutput);
      startCodexSession(firstHost, tmpdir(), 1);
      firstHost.handleCommand(
        command(
          "SendTurn",
          `generation-1-${interactionType}`,
          { inputText: `[${interactionType}]` },
          1,
        ),
      );

      const recoveredOutput: ProviderHostMessage[] = [];
      const recoveredHost = fixtureHost(recoveredOutput);
      startCodexSession(recoveredHost, tmpdir(), 2);
      recoveredHost.handleCommand(
        command(
          "SendTurn",
          `generation-2-${interactionType}`,
          { inputText: `[${interactionType}]` },
          2,
        ),
      );

      const firstRequestId = interactionRequestIdFor(
        firstOutput,
        `generation-1-${interactionType}`,
      );
      const recoveredRequestId = interactionRequestIdFor(
        recoveredOutput,
        `generation-2-${interactionType}`,
      );
      expect(firstRequestId).toBe(`fixture-${interactionType}-generation-1-1`);
      expect(recoveredRequestId).toBe(`fixture-${interactionType}-generation-2-1`);
      expect(recoveredRequestId).not.toBe(firstRequestId);
    },
  );

  it("supports authoritative ResumeSession and StopSession cancellation without enabling Local-only Providers", () => {
    const output: ProviderHostMessage[] = [];
    const host = fixtureHost(output);
    host.handleCommand(
      command("ResumeSession", "resume-1", {
        runnerInput: {
          workload: {
            provider: "claudeAgent",
            conversationHistory: [{ role: "user", text: "before" }],
          },
          workspaceDirectory: tmpdir(),
        },
        runtimeEventVersion: 2,
      }),
    );
    expect(messagesFor(output, "resume-1").at(-1)).toMatchObject({
      messageType: "Result",
      payload: { provider: "claudeAgent", resumed: true },
    });

    host.handleCommand(command("SendTurn", "send-stop", { inputText: "[user-input]" }));
    host.handleCommand(command("StopSession", "stop-1", {}));
    expect(messagesFor(output, "send-stop").at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "cancelled" },
    });
    expect(messagesFor(output, "stop-1").at(-1)).toMatchObject({
      messageType: "Result",
      payload: { stopped: true },
    });

    host.handleCommand(
      command("StartSession", "local-only", {
        runnerInput: { workload: { provider: "cursor" }, workspaceDirectory: tmpdir() },
      }),
    );
    expect(messagesFor(output, "local-only").at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "capability_unsupported" },
    });
  });

  it("returns stable Provider and unsupported errors and enforces commandId content identity", () => {
    const output: ProviderHostMessage[] = [];
    const host = fixtureHost(output);
    startCodexSession(host);

    const providerError = command("SendTurn", "provider-error", { inputText: "[provider-error]" });
    host.handleCommand(providerError);
    expect(messagesFor(output, "provider-error").at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "provider_rate_limited", retryable: true },
    });

    const compact = command("CompactSession", "compact-1", {});
    host.handleCommand(compact);
    expect(messagesFor(output, "compact-1").at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "capability_unsupported" },
    });

    const describe = command("Describe", "describe-replay", { provider: "codex" });
    host.handleCommand(describe);
    host.handleCommand(describe);
    expect(messagesFor(output, "describe-replay")[1]).toEqual(
      messagesFor(output, "describe-replay")[0],
    );
    host.handleCommand(command("Describe", "describe-replay", { provider: "claudeAgent" }));
    expect(messagesFor(output, "describe-replay").at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "protocol_violation" },
    });
  });

  it("rejects secret-bearing input without reflecting Credential, Worker, or Lease Token values", () => {
    const output: string[] = [];
    const host = new Stage3ProviderAcceptanceHost({
      enabledProviders: new Set(["codex"]),
      emitLine: (line) => output.push(line),
    });
    const secret = "stage3-super-secret-lease-token";

    host.receiveLine(
      JSON.stringify(
        command("StartSession", "secret-input", {
          runnerInput: {
            workload: { provider: "codex" },
            leaseToken: secret,
          },
        }),
      ),
    );

    expect(output.join("\n")).not.toContain(secret);
    expect(JSON.parse(output[0] ?? "{}")).toMatchObject({
      messageType: "Error",
      error: { code: "protocol_violation" },
    });
  });

  it("audits the anonymous Credential FD with boolean and key-only evidence", () => {
    const workspace = mkdtempSync(join(tmpdir(), "synara-stage3-credential-"));
    temporaryDirectories.push(workspace);
    const credentialPath = join(workspace, "credential.json");
    const secret = STAGE3_FIXTURE_CREDENTIAL_SENTINEL;
    writeFileSync(
      credentialPath,
      JSON.stringify({ payload: { acceptanceToken: secret, provider: "fixture" } }),
      { mode: 0o600 },
    );
    const descriptor = openSync(credentialPath, "r");
    const previousDescriptor = process.env.SYNARA_PROVIDER_CREDENTIAL_FD;
    process.env.SYNARA_PROVIDER_CREDENTIAL_FD = String(descriptor);
    const encoded: string[] = [];
    try {
      const host = new Stage3ProviderAcceptanceHost({
        enabledProviders: new Set(["codex"]),
        emitLine: (line) => encoded.push(line),
      });
      startCodexSession(host);
      host.handleCommand(command("SendTurn", "credential-turn", { inputText: "[credential]" }));
    } finally {
      if (previousDescriptor === undefined) delete process.env.SYNARA_PROVIDER_CREDENTIAL_FD;
      else process.env.SYNARA_PROVIDER_CREDENTIAL_FD = previousDescriptor;
    }

    expect(encoded.join("\n")).not.toContain(secret);
    expect(JSON.parse(encoded.at(-1) ?? "{}")).toMatchObject({
      messageType: "Result",
      payload: {
        output: {
          credentialEvidence: {
            credentialVerified: true,
            credentialPayloadKeys: ["acceptanceToken", "provider"],
          },
        },
      },
    });
  });

  it("runs as JSONL and exposes opt-in malformed and oversized protocol fault hooks", () => {
    const fixturePath = join(import.meta.dirname, "provider-host-fixture.ts");
    const describe = JSON.stringify(command("Describe", "describe-cli", { provider: "codex" }));
    const jsonlInput = [
      describe,
      JSON.stringify(
        command("StartSession", "session-cli", {
          runnerInput: { workload: { provider: "codex" }, workspaceDirectory: tmpdir() },
          runtimeEventVersion: 2,
        }),
      ),
      JSON.stringify(command("SendTurn", "send-cli", { inputText: "[approval]" })),
      JSON.stringify(
        command("ResolveApproval", "resolve-cli", {
          requestId: "fixture-approval-generation-1-1",
          resolution: { decision: "accept" },
        }),
      ),
    ].join("\n");
    const normal = spawnSync(
      "bun",
      ["run", fixturePath, "--protocol-v2", "--enable-providers=codex,claudeAgent"],
      { input: `${jsonlInput}\n`, encoding: "utf8" },
    );
    expect(normal.status).toBe(0);
    const normalMessages = normal.stdout
      .trim()
      .split("\n")
      .map((line) => decodeMessage(JSON.parse(line)));
    expect(normalMessages.map((message) => message.messageType)).toEqual([
      "Result",
      "Result",
      "InteractionRequest",
      "Result",
      "Result",
    ]);
    expect(messagesFor(normalMessages, "send-cli").filter(isTerminal)).toHaveLength(1);

    const malformed = spawnSync(
      "bun",
      ["run", fixturePath, "--fault=malformed", "--fault-on=Describe"],
      { input: `${describe}\n`, encoding: "utf8" },
    );
    expect(malformed.status).toBe(0);
    expect(malformed.stdout).toBe("{malformed-provider-host-jsonl\n");

    const oversized = spawnSync(
      "bun",
      ["run", fixturePath, "--fault=oversized", "--fault-on=Describe"],
      { input: `${describe}\n`, encoding: "utf8", maxBuffer: 2 * PROVIDER_HOST_MAX_MESSAGE_BYTES },
    );
    expect(oversized.status).toBe(0);
    expect(Buffer.byteLength(oversized.stdout.trim())).toBeGreaterThan(
      PROVIDER_HOST_MAX_MESSAGE_BYTES,
    );
  });

  it("parses composable scenario directives and defaults to text", () => {
    expect(parseFixtureScenarios("plain input")).toEqual(["text"]);
    expect(parseFixtureScenarios("[tool] fixture:usage [artifact] [workspace-verify]")).toEqual([
      "tool",
      "usage",
      "artifact",
      "workspace-verify",
    ]);
  });
});

function fixtureHost(output: ProviderHostMessage[]): Stage3ProviderAcceptanceHost {
  return new Stage3ProviderAcceptanceHost({
    enabledProviders: new Set(["codex", "claudeAgent"]),
    emitLine: (line) => output.push(decodeMessage(JSON.parse(line))),
  });
}

function startCodexSession(
  host: Stage3ProviderAcceptanceHost,
  workspaceDirectory = tmpdir(),
  generation = 1,
): void {
  host.handleCommand(
    command(
      "StartSession",
      "session-start",
      {
        runnerInput: { workload: { provider: "codex" }, workspaceDirectory },
        runtimeEventVersion: 2,
      },
      generation,
    ),
  );
}

function command(
  commandType: ProviderHostCommandEnvelope["commandType"],
  commandId: string,
  payload: Record<string, unknown>,
  generation = 1,
): ProviderHostCommandEnvelope {
  return {
    requestId: `request-${commandId}`,
    protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
    executionId: "fixture-execution",
    generation,
    commandType,
    commandId: commandId as ProviderHostCommandEnvelope["commandId"],
    occurredAt: "2026-07-14T00:00:00.000Z",
    payload,
  };
}

function messagesFor(output: ReadonlyArray<ProviderHostMessage>, commandId: string) {
  return output.filter((message) => message.commandId === commandId);
}

function interactionRequestIdFor(
  output: ReadonlyArray<ProviderHostMessage>,
  commandId: string,
): string | undefined {
  return messagesFor(output, commandId).find((message) => message.messageType === "InteractionRequest")
    ?.payload.requestId as string | undefined;
}

function isTerminal(message: ProviderHostMessage): boolean {
  return message.messageType === "Result" || message.messageType === "Error";
}

function eventTypes(messages: ReadonlyArray<ProviderHostMessage>): string[] {
  return messages.flatMap((message) =>
    message.messageType === "Event" ? [message.payload.eventType as string] : [],
  );
}
