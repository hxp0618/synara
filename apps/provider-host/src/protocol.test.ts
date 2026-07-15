import { PROVIDER_RUNTIME_EVENT_VERSION } from "@synara/contracts";
import {
  PROVIDER_CAPABILITY_CATALOG,
  PROVIDER_CAPABILITY_IDS,
  PROVIDER_HOST_PROTOCOL_VERSION,
  PROVIDER_HOST_PROVIDER_KINDS,
  type ProviderHostCommandEnvelope,
  type ProviderHostMessageEnvelope,
  type ProviderHostProviderKind,
} from "@synara/contracts/provider-host";
import { PassThrough } from "node:stream";
import { describe, expect, it } from "vitest";

import providerHostPackage from "../package.json";
import {
  capabilityMapForProvider,
  createProviderHostProtocolHandler,
  providerHostDescriptor,
  runProviderHostProtocolV2,
  type CodexVersionProbeResult,
} from "./protocol";
import type { ProviderRunController, RunnerMessage } from "./providerHost";

function command(
  commandType: ProviderHostCommandEnvelope["commandType"],
  payload: Record<string, unknown>,
  commandId = `command-${commandType}`,
  generation = 1,
): ProviderHostCommandEnvelope {
  return {
    requestId: `request-${commandType}`,
    protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
    executionId: "execution-1",
    generation,
    commandType,
    commandId: commandId as ProviderHostCommandEnvelope["commandId"],
    occurredAt: "2026-07-13T02:00:00.000Z",
    payload,
  };
}

describe("Provider Host Protocol v2", () => {
  it("describes the fixed ordered 8 by 28 Provider matrix from the catalog", () => {
    for (const provider of PROVIDER_HOST_PROVIDER_KINDS) {
      const descriptor = enabledDescriptorForProvider(provider);
      const catalogEntry = PROVIDER_CAPABILITY_CATALOG.providers.find(
        (entry) => entry.provider === provider,
      );

      expect(catalogEntry).toBeDefined();
      expect(descriptor.protocolVersion).toEqual({ major: 2, minor: 1 });
      expect(descriptor.capabilityDescriptor).toMatchObject({
        provider,
        supportTier: catalogEntry?.supportTier,
        adapterVersion: catalogEntry?.adapterVersion,
      });
      expect(Object.keys(descriptor.capabilityDescriptor.capabilities)).toEqual(
        PROVIDER_CAPABILITY_IDS,
      );
      expect(descriptor.capabilityDescriptor.capabilities).toEqual(catalogEntry?.capabilities);
      expect(capabilityMapForProvider(provider)).toEqual(catalogEntry?.capabilities);
    }
  });

  it("keeps Experimental Providers disabled by default and separates Local-only policy", () => {
    const codexDisabled = providerHostDescriptor("codex", {
      environment: {},
      codexVersionProbe: compatibleCodexProbe,
    });
    const claudeDisabled = providerHostDescriptor("claudeAgent", { environment: {} });
    const codexEnabled = providerHostDescriptor("codex", {
      environment: {
        SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: " codex, claudeAgent ",
      },
      codexVersionProbe: compatibleCodexProbe,
    });
    const claudeEnabled = providerHostDescriptor("claudeAgent", {
      environment: { SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "claude" },
    });
    const cursor = providerHostDescriptor("cursor", {
      environment: { SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "cursor" },
    });

    expect(codexDisabled.capabilityDescriptor.releasePolicy).toEqual({
      requiresExplicitEnablement: true,
      enabled: false,
    });
    expect(claudeDisabled.capabilityDescriptor.releasePolicy.enabled).toBe(false);
    expect(codexEnabled.capabilityDescriptor.releasePolicy.enabled).toBe(true);
    expect(claudeEnabled.capabilityDescriptor.releasePolicy.enabled).toBe(true);
    expect(cursor.capabilityDescriptor).toMatchObject({
      supportTier: "local-only",
      releasePolicy: { requiresExplicitEnablement: false, enabled: true },
    });
  });

  it("uses Codex CLI and Claude bundle metadata as independent Runtime sources", () => {
    const codex = enabledDescriptorForProvider("codex");
    const claude = providerHostDescriptor("claudeAgent", {
      environment: { SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "claudeAgent" },
      codexVersionProbe: () => {
        throw new Error("Claude descriptor must not execute the Codex or Claude CLI probe.");
      },
    });

    expect(codex.capabilityDescriptor.providerCliVersion).toBe("0.144.1");
    expect(codex.capabilityDescriptor.runtime).toEqual({
      kind: "cli",
      name: "codex",
      version: "0.144.1",
      available: true,
      versionSource: "probe",
      compatibleRange: {
        minimumInclusive: "0.144.1",
        maximumExclusive: "0.145.0",
      },
      compatible: true,
    });
    expect(claude.capabilityDescriptor.providerCliVersion).toBeUndefined();
    expect(claude.capabilityDescriptor.runtime).toMatchObject({
      kind: "sdk",
      name: "@anthropic-ai/claude-agent-sdk",
      version: "0.3.207",
      available: true,
      versionSource: "package",
      compatible: true,
    });
    expect(codex.runtimeEventVersions).toEqual({
      minimum: PROVIDER_RUNTIME_EVENT_VERSION,
      maximum: PROVIDER_RUNTIME_EVENT_VERSION,
    });
  });

  it("uses package build metadata instead of ambient build-version environment", () => {
    const descriptor = providerHostDescriptor("cursor", {
      environment: { SYNARA_PROVIDER_HOST_BUILD_VERSION: "ambient-build-must-not-win" },
    });

    expect(descriptor.hostBuildVersion).toBe(providerHostPackage.version);
    expect(descriptor.capabilityDescriptor.runtime?.version).toBe(providerHostPackage.version);
  });

  it("returns a versioned Describe result and replays the same terminal by commandId", async () => {
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
      descriptorForProvider: enabledDescriptorForProvider,
    });
    const describe = command("Describe", { provider: "codex" }, "describe-1");

    const first = await handle(describe);
    const second = await handle(describe);

    expect(first.at(-1)?.messageType).toBe("Result");
    expect(second).toEqual([first.at(-1)]);
    expect(emitted).toHaveLength(2);
  });

  it("rejects a Local-only Provider before execution", async () => {
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
      descriptorForProvider: (provider) =>
        providerHostDescriptor(provider, {
          environment: { SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "cursor" },
          codexVersionProbe: compatibleCodexProbe,
        }),
    });
    const result = await handle(
      command("StartSession", {
        runnerInput: {
          execution: { id: "execution-1" },
          workload: { provider: "cursor", inputText: "unused" },
          workspaceDirectory: "/tmp/workspace",
        },
      }),
    );

    const terminal = result.at(-1);
    expect(terminal?.messageType).toBe("Error");
    if (terminal?.messageType === "Error") {
      expect(terminal.error.code).toBe("capability_unsupported");
    }
  });

  it.each(["StartSession", "ResumeSession"] as const)(
    "fails closed for %s when the Experimental Provider is disabled",
    async (commandType) => {
      const handle = createProviderHostProtocolHandler({
        credential: null,
        emit: () => {},
        descriptorForProvider: (provider) =>
          providerHostDescriptor(provider, {
            environment: {},
            codexVersionProbe: compatibleCodexProbe,
          }),
      });
      const result = await handle(
        command(
          commandType,
          { runnerInput: remoteRunnerInput(commandType === "ResumeSession") },
          `disabled-${commandType}`,
        ),
      );

      expect(errorCode(result)).toBe("capability_unsupported");
    },
  );

  it("accepts ResumeSession when ResumeSnapshot provides authoritative history without a native Cursor", async () => {
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: enabledDescriptorForProvider,
    });

    const result = await handle(
      command("ResumeSession", {
        runnerInput: {
          ...remoteRunnerInput(false),
          workload: {
            provider: "codex",
            inputText: "continue",
            resumeSnapshot: {
              version: 1,
              sessionId: "session-1",
              turnId: "turn-2",
              provider: "codex",
              messages: [{ role: "user", text: "prior question" }],
            },
          },
        },
      }),
    );

    expect(result.at(-1)?.messageType).toBe("Result");
  });

  it.each(["StartSession", "ResumeSession"] as const)(
    "binds %s runner input to the command Generation",
    async (commandType) => {
      let receivedGeneration: number | undefined;
      const handle = createProviderHostProtocolHandler({
        credential: null,
        emit: () => {},
        descriptorForProvider: enabledDescriptorForProvider,
        startRun: (input) => {
          receivedGeneration = input.execution.generation;
          return {
            result: Promise.resolve({ type: "result", output: { text: "done" } }),
            interrupt: () => {},
          } satisfies ProviderRunController;
        },
      });
      await handle(
        command(
          commandType,
          { runnerInput: remoteRunnerInput(commandType === "ResumeSession") },
          `generation-session-${commandType}`,
          7,
        ),
      );

      await handle(
        command("SendTurn", { inputText: "continue" }, `generation-turn-${commandType}`, 7),
      );

      expect(receivedGeneration).toBe(7);
    },
  );

  it.each(["StartSession", "ResumeSession"] as const)(
    "rejects %s when runner input explicitly names a different Generation",
    async (commandType) => {
      const handle = createProviderHostProtocolHandler({
        credential: null,
        emit: () => {},
        descriptorForProvider: enabledDescriptorForProvider,
      });
      const runnerInput = remoteRunnerInput(commandType === "ResumeSession");
      const result = await handle(
        command(
          commandType,
          {
            runnerInput: {
              ...runnerInput,
              execution: { ...runnerInput.execution, generation: 6 },
            },
          },
          `generation-mismatch-${commandType}`,
          7,
        ),
      );

      expect(result.at(-1)).toMatchObject({
        messageType: "Error",
        error: {
          code: "protocol_violation",
          message: "runnerInput.execution.generation does not match command.generation.",
        },
      });
    },
  );

  it.each(["StartSession", "ResumeSession"] as const)(
    "fails closed for %s when the Runtime version is incompatible",
    async (commandType) => {
      const handle = createProviderHostProtocolHandler({
        credential: null,
        emit: () => {},
        descriptorForProvider: codexDescriptorFactory({
          available: true,
          output: "codex-cli 0.145.0",
        }),
      });
      const result = await handle(
        command(
          commandType,
          { runnerInput: remoteRunnerInput(commandType === "ResumeSession") },
          `incompatible-${commandType}`,
        ),
      );

      expect(errorCode(result)).toBe("provider_version_incompatible");
    },
  );

  it("enforces the Codex Runtime availability and exact compatible range", async () => {
    const cases = [
      {
        label: "unavailable",
        probe: { available: false },
        expected: "provider_not_installed",
      },
      {
        label: "unverifiable",
        probe: { available: true, output: "codex-cli unknown" },
        expected: "provider_version_incompatible",
      },
      {
        label: "unstable-semver",
        probe: { available: true, output: "codex-cli 0.144.1-beta.1" },
        expected: "provider_version_incompatible",
      },
      {
        label: "below-minimum",
        probe: { available: true, output: "codex-cli 0.144.0" },
        expected: "provider_version_incompatible",
      },
      {
        label: "minimum",
        probe: { available: true, output: "codex-cli 0.144.1" },
        expected: "Result",
      },
      {
        label: "compatible-patch",
        probe: { available: true, output: "codex-cli 0.144.99" },
        expected: "Result",
      },
      {
        label: "maximum-exclusive",
        probe: { available: true, output: "codex-cli 0.145.0" },
        expected: "provider_version_incompatible",
      },
    ] as const;

    for (const testCase of cases) {
      const handle = createProviderHostProtocolHandler({
        credential: null,
        emit: () => {},
        descriptorForProvider: codexDescriptorFactory(testCase.probe),
      });
      const result = await handle(
        command("StartSession", { runnerInput: remoteRunnerInput() }, `runtime-${testCase.label}`),
      );
      const terminal = result.at(-1);

      if (testCase.expected === "Result") {
        expect(terminal?.messageType, testCase.label).toBe("Result");
      } else {
        expect(errorCode(result), testCase.label).toBe(testCase.expected);
      }
    }
  });

  it("processes InterruptTurn while SendTurn is still active", async () => {
    let rejectRun: ((error: Error) => void) | undefined;
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: () =>
        ({
          result: new Promise((_, reject) => {
            rejectRun = reject;
          }),
          interrupt: () => rejectRun?.(new Error("Provider turn was interrupted.")),
          getResumeCursor: () => "provider-cursor-after-interrupt",
        }) satisfies ProviderRunController,
    });
    await handle(
      command("StartSession", { runnerInput: remoteRunnerInput() }, "session-interrupt"),
    );

    const send = handle(command("SendTurn", { inputText: "long task" }, "send-interrupt"));
    const interrupt = await handle(
      command("InterruptTurn", { targetCommandId: "send-interrupt" }, "interrupt-active"),
    );
    const sendMessages = await send;

    expect(interrupt.at(-1)).toMatchObject({
      messageType: "Result",
      payload: {
        interrupted: true,
        targetCommandId: "send-interrupt",
        providerResumeCursor: "provider-cursor-after-interrupt",
      },
    });
    expect(sendMessages.at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "interrupted" },
    });
  });

  it("runs native CompactSession as the sole active primary operation and exposes its boundary", async () => {
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    let operation: unknown;
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: (_input, _credential, _emit, options) => {
        operation = options?.operation;
        return {
          result: new Promise((resolve) => {
            completeRun = resolve;
          }),
          interrupt: () => {},
        } satisfies ProviderRunController;
      },
    });
    await handle(
      command("ResumeSession", { runnerInput: remoteRunnerInput(true) }, "session-compact"),
    );

    const compact = handle(command("CompactSession", {}, "compact-active"));
    expect(operation).toEqual({ commandType: "CompactSession", payload: {} });
    completeRun?.({
      type: "result",
      output: {
        operation: "compact",
        supportMode: "native",
        boundary: {
          kind: "context_compaction",
          summaryAvailable: false,
          detail: "Codex did not expose a summary.",
        },
      },
      providerResumeCursor: "provider-cursor",
    });

    await expect(compact).resolves.toEqual([
      expect.objectContaining({
        messageType: "Result",
        payload: {
          output: expect.objectContaining({ operation: "compact", supportMode: "native" }),
          providerResumeCursor: "provider-cursor",
          supportMode: "native",
          boundary: expect.objectContaining({
            kind: "context_compaction",
            summaryAvailable: false,
          }),
        },
      }),
    ]);
  });

  it("targets InterruptTurn at an active primary operation", async () => {
    let rejectRun: ((error: Error) => void) | undefined;
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: () =>
        ({
          result: new Promise((_, reject) => {
            rejectRun = reject;
          }),
          interrupt: () => rejectRun?.(new Error("Provider operation was interrupted.")),
          getResumeCursor: () => "provider-cursor-after-primary-interrupt",
        }) satisfies ProviderRunController,
    });
    await handle(
      command(
        "ResumeSession",
        { runnerInput: remoteRunnerInput(true) },
        "session-primary-interrupt",
      ),
    );

    const compact = handle(command("CompactSession", {}, "compact-interrupt"));
    const interrupt = await handle(
      command("InterruptTurn", { targetCommandId: "compact-interrupt" }, "interrupt-primary"),
    );

    expect(interrupt.at(-1)).toMatchObject({
      messageType: "Result",
      payload: {
        interrupted: true,
        targetCommandId: "compact-interrupt",
        providerResumeCursor: "provider-cursor-after-primary-interrupt",
      },
    });
    expect((await compact).at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "interrupted" },
    });
  });

  it("keeps Claude manual compact and Provider-native rollback/fork stably unsupported", async () => {
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: enabledDescriptorForProvider,
    });
    const claudeInput = {
      ...remoteRunnerInput(),
      workload: { provider: "claudeAgent", inputText: "initial" },
    };
    await handle(command("StartSession", { runnerInput: claudeInput }, "session-claude-compact"));
    expect(errorCode(await handle(command("CompactSession", {}, "compact-claude")))).toBe(
      "capability_unsupported",
    );

    for (const commandType of ["RollbackSession", "ForkSession"] as const) {
      expect(errorCode(await handle(command(commandType, {}, `unsupported-${commandType}`)))).toBe(
        "capability_unsupported",
      );
    }
  });

  it("emits only canonical Runtime Event v2 payloads on the v2 wire", async () => {
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: (_input, _credential, emit) => {
        emit({
          type: "event",
          eventType: "runtime.output.delta",
          payload: { text: "canonical" },
        });
        return {
          result: new Promise((resolve) => {
            completeRun = resolve;
          }),
          interrupt: () => {},
        } satisfies ProviderRunController;
      },
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-events"));

    const send = handle(command("SendTurn", { inputText: "stream" }, "send-events"));
    completeRun?.({ type: "result", output: { text: "canonical" } });
    await send;

    expect(emitted).toContainEqual(
      expect.objectContaining({
        commandId: "send-events",
        messageType: "Event",
        payload: {
          eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
          eventType: "content.delta",
          payload: { streamKind: "assistant_text", delta: "canonical" },
        },
      }),
    );
  });

  it("maps Runner Artifacts to Provider Host ArtifactCandidate messages", async () => {
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: (_input, _credential, emit) => {
        emit({
          type: "artifact",
          artifact: {
            path: "tool-results/terminal.log",
            kind: "terminal_log",
            originalName: "claude-terminal.log",
            contentType: "text/plain",
            sourceRoot: "runtime-output",
            terminalId: "terminal-1",
            encoding: "utf-8",
            reportedSize: 8_192,
          },
        });
        return {
          result: new Promise((resolve) => {
            completeRun = resolve;
          }),
          interrupt: () => {},
        } satisfies ProviderRunController;
      },
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-artifact"));

    const send = handle(command("SendTurn", { inputText: "produce output" }, "send-artifact"));
    completeRun?.({ type: "result", output: { text: "done" } });
    await send;

    expect(emitted).toContainEqual(
      expect.objectContaining({
        commandId: "send-artifact",
        messageType: "ArtifactCandidate",
        payload: {
          artifact: {
            path: "tool-results/terminal.log",
            kind: "terminal_log",
            originalName: "claude-terminal.log",
            contentType: "text/plain",
            sourceRoot: "runtime-output",
            terminalId: "terminal-1",
            encoding: "utf-8",
            reportedSize: 8_192,
          },
        },
      }),
    );
  });

  it("processes SteerTurn while SendTurn remains active", async () => {
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    let steeredPayload: Record<string, unknown> | undefined;
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: () =>
        ({
          result: new Promise((resolve) => {
            completeRun = resolve;
          }),
          interrupt: () => {},
          steer: (payload) => {
            steeredPayload = payload;
          },
        }) satisfies ProviderRunController,
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-steer"));

    const send = handle(command("SendTurn", { inputText: "long task" }, "send-steer"));
    const steer = await handle(
      command(
        "SteerTurn",
        { targetCommandId: "send-steer", inputText: "focus on tests" },
        "steer-active",
      ),
    );
    completeRun?.({ type: "result", output: { text: "done" } });
    const sendMessages = await send;

    expect(steeredPayload).toEqual({ inputText: "focus on tests" });
    expect(steer.at(-1)).toMatchObject({
      messageType: "Result",
      payload: { steered: true, targetCommandId: "send-steer" },
    });
    expect(sendMessages.at(-1)).toMatchObject({
      messageType: "Result",
      payload: { output: { text: "done" } },
    });
  });

  it("delivers a correlated approval resolution during an active SendTurn", async () => {
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    let resolvedPayload: Record<string, unknown> | undefined;
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: (_input, _credential, emit) => {
        emit({
          type: "interaction",
          interactionType: "approval",
          payload: { requestId: "approval-1", summary: "Run command" },
        });
        return {
          result: new Promise((resolve) => {
            completeRun = resolve;
          }),
          interrupt: () => {},
          resolveApproval: (payload) => {
            resolvedPayload = payload;
            completeRun?.({ type: "result", output: { text: "approved" } });
          },
        } satisfies ProviderRunController;
      },
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-approval"));

    const send = handle(command("SendTurn", { inputText: "needs approval" }, "send-approval"));
    const resolution = await handle(
      command(
        "ResolveApproval",
        { requestId: "approval-1", resolution: { decision: "accept" } },
        "approval-1:resolution",
      ),
    );
    const sendMessages = await send;

    expect(emitted).toContainEqual(
      expect.objectContaining({
        commandId: "send-approval",
        messageType: "InteractionRequest",
        payload: expect.objectContaining({ interactionType: "approval", requestId: "approval-1" }),
      }),
    );
    expect(resolvedPayload).toEqual({
      requestId: "approval-1",
      resolution: { decision: "accept" },
    });
    expect(resolution.at(-1)?.messageType).toBe("Result");
    expect(sendMessages.at(-1)).toMatchObject({
      messageType: "Result",
      payload: { output: { text: "approved" } },
    });
  });

  it("keeps reading stdin commands while SendTurn is pending", async () => {
    const source = new PassThrough();
    const emitted: ProviderHostMessageEnvelope[] = [];
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    const protocol = runProviderHostProtocolV2({
      source,
      credential: null,
      emit: (message) => emitted.push(message),
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: (_input, _credential, emit) => {
        emit({
          type: "interaction",
          interactionType: "approval",
          payload: { requestId: "approval-stream" },
        });
        return {
          result: new Promise((resolve) => {
            completeRun = resolve;
          }),
          interrupt: () => {},
          resolveApproval: () => {
            completeRun?.({ type: "result", output: { text: "stream-approved" } });
          },
        } satisfies ProviderRunController;
      },
    });

    for (const item of [
      command("StartSession", { runnerInput: remoteRunnerInput() }, "session-stream"),
      command("SendTurn", { inputText: "stream task" }, "send-stream"),
      command(
        "ResolveApproval",
        { requestId: "approval-stream", resolution: { decision: "accept" } },
        "approval-stream:resolution",
      ),
    ]) {
      source.write(`${JSON.stringify(item)}\n`);
    }
    source.end();
    await protocol;

    expect(emitted).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ commandId: "approval-stream:resolution", messageType: "Result" }),
        expect.objectContaining({
          commandId: "send-stream",
          messageType: "Result",
          payload: { output: { text: "stream-approved" } },
        }),
      ]),
    );
  });
});

function compatibleCodexProbe(): CodexVersionProbeResult {
  return { available: true, output: "codex-cli 0.144.1" };
}

function enabledDescriptorForProvider(provider: ProviderHostProviderKind) {
  return providerHostDescriptor(provider, {
    environment: {
      SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "codex,claudeAgent",
    },
    codexVersionProbe: compatibleCodexProbe,
  });
}

function codexDescriptorFactory(probe: CodexVersionProbeResult) {
  return (provider: ProviderHostProviderKind) =>
    providerHostDescriptor(provider, {
      environment: { SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "codex" },
      codexVersionProbe: () => probe,
    });
}

function errorCode(messages: ReadonlyArray<ProviderHostMessageEnvelope>): string | undefined {
  const terminal = messages.at(-1);
  return terminal?.messageType === "Error" ? terminal.error.code : terminal?.messageType;
}

function remoteRunnerInput(resume = false) {
  return {
    execution: { id: "execution-1" },
    workload: { provider: "codex", inputText: "initial" },
    workspaceDirectory: "/tmp/workspace",
    ...(resume ? { providerResumeCursor: "provider-cursor" } : {}),
  };
}
