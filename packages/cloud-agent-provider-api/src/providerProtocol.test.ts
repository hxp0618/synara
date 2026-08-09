import {
  CLOUD_AGENT_RUNTIME_EVENT_VERSION,
  CLOUD_AGENT_PROVIDER_CAPABILITY_CATALOG as PROVIDER_CAPABILITY_CATALOG,
  CLOUD_AGENT_CAPABILITY_IDS as PROVIDER_CAPABILITY_IDS,
  CLOUD_AGENT_PROTOCOL_VERSION as PROVIDER_HOST_PROTOCOL_VERSION,
  type CloudAgentCommandEnvelope as ProviderHostCommandEnvelope,
  type CloudAgentMessageEnvelope as ProviderHostMessageEnvelope,
} from "@synara/cloud-agent-protocol";
import { PassThrough } from "node:stream";
import { describe, expect, it } from "vitest";

import providerHostPackage from "../package.json";
import {
  capabilityMapForProvider,
  createProviderHostProtocolHandler,
  providerHostDescriptor,
  runProviderHostProtocolV2,
  type ProviderVersionProbeResult,
} from "./providerProtocol";
import type { ProviderRunController, RunnerInput, RunnerMessage } from "./internalExecution";
import { ProviderInterruptedError } from "./providerRunErrors";

type ProviderHostProviderKind = string;
const PROVIDER_HOST_PROVIDER_KINDS = PROVIDER_CAPABILITY_CATALOG.providers.map(
  (entry) => entry.provider,
);

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
  it("describes the fixed ordered 8 by 29 Provider matrix from the catalog", () => {
    for (const provider of PROVIDER_HOST_PROVIDER_KINDS) {
      const descriptor = enabledDescriptorForProvider(provider);
      const catalogEntry = PROVIDER_CAPABILITY_CATALOG.providers.find(
        (entry) => entry.provider === provider,
      );

      expect(catalogEntry).toBeDefined();
      expect(descriptor.protocolVersion).toEqual({ major: 2, minor: 3 });
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

  it("advertises native suspend-active-turn for Codex and Claude Agent", () => {
    const providers = new Map(
      PROVIDER_CAPABILITY_CATALOG.providers.map((entry) => [entry.provider, entry] as const),
    );

    expect(providers.get("codex")?.capabilities["suspend-active-turn"]).toBe("native");
    expect(providers.get("claudeAgent")?.capabilities["suspend-active-turn"]).toBe("native");
  });

  it("keeps Experimental Providers disabled by default and separates Local-only policy", () => {
    const codexDisabled = providerHostDescriptor("codex", {
      environment: {},
      runtimeVersionProbe: compatibleCodexProbe,
    });
    const claudeDisabled = providerHostDescriptor("claudeAgent", { environment: {} });
    const codexEnabled = providerHostDescriptor("codex", {
      environment: {
        SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: " codex, claudeAgent ",
      },
      runtimeVersionProbe: compatibleCodexProbe,
    });
    const claudeEnabled = providerHostDescriptor("claudeAgent", {
      environment: { SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "claudeAgent" },
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
      runtimeVersion: "0.3.207",
      runtimeVersionProbe: () => {
        throw new Error("Claude descriptor must not execute the Codex or Claude CLI probe.");
      },
    });

    expect(codex.capabilityDescriptor.providerCliVersion).toBe("0.145.0");
    expect(codex.capabilityDescriptor.runtime).toEqual({
      kind: "cli",
      name: "codex",
      version: "0.145.0",
      available: true,
      versionSource: "probe",
      compatibleRange: {
        minimumInclusive: "0.145.0",
        maximumExclusive: "0.146.0",
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
      minimum: CLOUD_AGENT_RUNTIME_EVENT_VERSION,
      maximum: CLOUD_AGENT_RUNTIME_EVENT_VERSION,
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

  it("allows StartSession to bind a workspace before the first Turn has input", async () => {
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => undefined,
      descriptorForProvider: enabledDescriptorForProvider,
    });
    const runnerInput = remoteRunnerInput();
    const messages = await handle(
      command("StartSession", {
        runnerInput: {
          ...runnerInput,
          workload: { ...runnerInput.workload, inputText: "" },
        },
      }),
    );

    expect(messages.at(-1)).toMatchObject({
      messageType: "Result",
      payload: { provider: "codex", resumed: false },
    });
  });

  it("rejects a Local-only Provider before execution", async () => {
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
      descriptorForProvider: (provider) =>
        providerHostDescriptor(provider, {
          environment: { SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "cursor" },
          runtimeVersionProbe: compatibleCodexProbe,
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

  it("preserves an open Provider slug before generic descriptor rejection", async () => {
    let describedProvider: ProviderHostProviderKind | null = null;
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: (provider) => {
        describedProvider = provider;
        return providerHostDescriptor(provider, { environment: {} });
      },
    });

    const result = await handle(
      command("StartSession", {
        runnerInput: {
          execution: { id: "execution-1" },
          workload: { provider: "gemini", inputText: "unused" },
          workspaceDirectory: "/tmp/workspace",
        },
      }),
    );

    expect(describedProvider).toBe("gemini");
    expect(errorCode(result)).toBe("provider_unavailable");
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
            runtimeVersionProbe: compatibleCodexProbe,
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
      startRun: () => ({
        result: Promise.resolve({
          type: "result",
          output: { provider: "claudeAgent", text: "ok" },
        }),
        interrupt: () => undefined,
      }),
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
          output: "codex-cli 0.146.0",
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
        probe: { available: true, output: "codex-cli 0.145.0-beta.1" },
        expected: "provider_version_incompatible",
      },
      {
        label: "below-minimum",
        probe: { available: true, output: "codex-cli 0.144.0" },
        expected: "provider_version_incompatible",
      },
      {
        label: "minimum",
        probe: { available: true, output: "codex-cli 0.145.0" },
        expected: "Result",
      },
      {
        label: "compatible-patch",
        probe: { available: true, output: "codex-cli 0.145.99" },
        expected: "Result",
      },
      {
        label: "maximum-exclusive",
        probe: { available: true, output: "codex-cli 0.146.0" },
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

  it("quiesces an old Turn before allowing a replacement Session", async () => {
    let completeOldRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    const runInputs: RunnerInput[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => undefined,
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: (runnerInput) => {
        runInputs.push(runnerInput);
        if (runInputs.length === 1) {
          return {
            result: new Promise((resolve) => {
              completeOldRun = resolve;
            }),
            interrupt: () => undefined,
          } satisfies ProviderRunController;
        }
        return {
          result: Promise.resolve({ type: "result", output: { text: "new answer" } }),
          interrupt: () => undefined,
        } satisfies ProviderRunController;
      },
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-old"));
    const oldTurn = handle(command("SendTurn", { inputText: "old question" }, "turn-old"));

    const stop = handle(command("StopSession", {}, "stop-old"));
    let stopResolved = false;
    void stop.then(() => {
      stopResolved = true;
    });
    await Promise.resolve();
    expect(stopResolved).toBe(false);

    const overlappingStart = await handle(
      command("StartSession", { runnerInput: remoteRunnerInput() }, "session-too-early"),
    );
    expect(overlappingStart.at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "protocol_violation" },
    });

    completeOldRun?.({ type: "result", output: { text: "old answer" } });
    await oldTurn;
    expect(await stop).toEqual([
      expect.objectContaining({
        messageType: "Result",
        payload: expect.objectContaining({
          stopped: true,
          outcome: "quiesced",
          quiesced: true,
          graceful: true,
        }),
      }),
    ]);

    const replacementInput = {
      ...remoteRunnerInput(),
      workload: {
        ...remoteRunnerInput().workload,
        conversationHistory: [{ role: "user" as const, text: "replacement history" }],
      },
    };
    await handle(command("StartSession", { runnerInput: replacementInput }, "session-replacement"));
    await handle(command("SendTurn", { inputText: "new question" }, "turn-new"));

    expect(runInputs[1]?.workload.conversationHistory).toEqual([
      { role: "user", text: "replacement history" },
    ]);
  });

  it("does not resolve SuspendTurn before the active SendTurn reaches an interrupted terminal", async () => {
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
          interrupt: () => {},
          getResumeCursor: () => "provider-cursor-after-suspend",
        }) satisfies ProviderRunController,
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-suspend"));

    const send = handle(command("SendTurn", { inputText: "long task" }, "send-suspend"));
    const suspend = handle(
      command("SuspendTurn", { targetCommandId: "send-suspend" }, "suspend-active"),
    );

    let resolved = false;
    void suspend.then(() => {
      resolved = true;
    });
    await Promise.resolve();
    expect(resolved).toBe(false);

    rejectRun?.(new ProviderInterruptedError());

    expect((await suspend).at(-1)).toMatchObject({
      messageType: "Result",
      payload: {
        quiesced: true,
        targetCommandId: "send-suspend",
        checkpointProtocol: "provider-host-suspend-terminal-v1",
        providerResumeCursor: "provider-cursor-after-suspend",
      },
    });
    expect((await send).at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "interrupted" },
    });
  });

  it("fails SuspendTurn closed when the interrupted terminal lacks a resume cursor", async () => {
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
          interrupt: () => {},
          getResumeCursor: () => undefined,
        }) satisfies ProviderRunController,
    });
    await handle(
      command("ResumeSession", { runnerInput: remoteRunnerInput(true) }, "session-suspend-cursor"),
    );

    const send = handle(command("SendTurn", { inputText: "long task" }, "send-suspend-cursor"));
    const suspend = handle(
      command("SuspendTurn", { targetCommandId: "send-suspend-cursor" }, "suspend-missing-cursor"),
    );

    rejectRun?.(new ProviderInterruptedError());

    expect((await suspend).at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "provider_unavailable" },
    });
    expect((await send).at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "interrupted" },
    });
  });

  it("fails SuspendTurn closed when the active SendTurn ends without an interrupted terminal", async () => {
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
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
          getResumeCursor: () => "provider-cursor-after-natural-completion",
        }) satisfies ProviderRunController,
    });
    await handle(
      command("ResumeSession", { runnerInput: remoteRunnerInput(true) }, "session-suspend-race"),
    );

    const send = handle(command("SendTurn", { inputText: "long task" }, "send-suspend-race"));
    const suspend = handle(
      command("SuspendTurn", { targetCommandId: "send-suspend-race" }, "suspend-race"),
    );

    completeRun?.({ type: "result", output: { text: "completed normally" } });

    expect((await suspend).at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "provider_unavailable" },
    });
    expect((await send).at(-1)).toMatchObject({
      messageType: "Result",
      payload: { output: { text: "completed normally" } },
    });
  });

  it.each([
    {
      message: "HTTP 429 Too Many Requests: rate_limit_error",
      code: "provider_rate_limited",
      retryable: true,
      requiresNewExecution: true,
      requiresUserAction: false,
    },
    {
      message: "Status code 401 Unauthorized: invalid API key",
      code: "authentication_required",
      retryable: false,
      requiresNewExecution: false,
      requiresUserAction: true,
    },
    {
      message: "Provider Credential payload is invalid",
      code: "credential_invalid",
      retryable: false,
      requiresNewExecution: false,
      requiresUserAction: true,
    },
    {
      message: "Authoritative history reconstruction failed",
      code: "provider_unavailable",
      retryable: true,
      requiresNewExecution: true,
      requiresUserAction: false,
    },
  ])(
    "classifies real Provider failure text without treating authoritative as authentication: $code",
    async ({ message, code, retryable, requiresNewExecution, requiresUserAction }) => {
      const handle = createProviderHostProtocolHandler({
        credential: null,
        emit: () => {},
        descriptorForProvider: enabledDescriptorForProvider,
        startRun: () => ({
          result: Promise.reject(new Error(message)),
          interrupt: () => {},
        }),
      });
      await handle(
        command("StartSession", { runnerInput: remoteRunnerInput() }, `session-${code}`),
      );

      const result = await handle(command("SendTurn", { inputText: "fail" }, `send-${code}`));

      expect(result.at(-1)).toMatchObject({
        messageType: "Error",
        error: {
          code,
          retryable,
          requiresNewExecution,
          requiresUserAction,
          canReconstructFromHistory: code !== "credential_invalid",
          canMoveWorker: code !== "credential_invalid",
        },
      });
    },
  );

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

  it("runs Protocol 2.3 GenerateText in an isolated Provider execution", async () => {
    let observedInput: unknown;
    let observedOptions: unknown;
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: (input, _credential, _emit, options) => {
        observedInput = input;
        observedOptions = options;
        return {
          result: Promise.resolve({
            type: "result",
            output: { text: '{"task":"thread-title","title":"Portable agents"}' },
          }),
          interrupt: () => {},
        } satisfies ProviderRunController;
      },
    });
    await handle(
      command("ResumeSession", { runnerInput: remoteRunnerInput(true) }, "session-generate-text"),
    );

    const result = await handle(
      command(
        "GenerateText",
        {
          task: "thread-title",
          model: "gpt-test",
          input: { message: "Design portable Cloud Agents" },
        },
        "generate-title",
      ),
    );

    expect(observedInput).toMatchObject({
      execution: { id: "execution-1:text:generate-title" },
      workload: {
        model: "gpt-test",
        conversationHistory: [],
        resumeSnapshot: null,
      },
    });
    expect(observedInput).not.toHaveProperty("providerResumeCursor");
    expect(observedOptions).toEqual({
      interactive: false,
      operation: {
        commandType: "GenerateText",
        payload: {
          task: "thread-title",
          model: "gpt-test",
          input: { message: "Design portable Cloud Agents" },
        },
      },
    });
    expect(result.at(-1)).toMatchObject({
      messageType: "Result",
      payload: {
        result: { task: "thread-title", title: "Portable agents" },
      },
    });
  });

  it("tracks GenerateText as an active operation and StopSession interrupts it before quiescing", async () => {
    let rejectGeneration: ((error: Error) => void) | undefined;
    let interrupts = 0;
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: () => ({
        result: new Promise((_, reject) => {
          rejectGeneration = reject;
        }),
        interrupt: () => {
          interrupts += 1;
          rejectGeneration?.(new ProviderInterruptedError());
        },
      }),
    });
    await handle(
      command("StartSession", { runnerInput: remoteRunnerInput() }, "session-generate-stop"),
    );
    const generation = handle(
      command(
        "GenerateText",
        { task: "branch-name", input: { message: "Long metadata generation" } },
        "generate-stop",
      ),
    );

    const stop = await handle(command("StopSession", {}, "stop-generation"));

    expect(interrupts).toBe(1);
    expect((await generation).at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "interrupted" },
    });
    expect(stop.at(-1)).toMatchObject({
      messageType: "Result",
      payload: { outcome: "quiesced", quiesced: true, graceful: true },
    });
  });

  it.each([
    {
      name: "timed-out",
      forceStop: undefined,
      expected: "timed-out",
    },
    {
      name: "failed",
      forceStop: () => {
        throw new Error("forced teardown failed");
      },
      expected: "failed",
    },
  ])("reports a stable $name StopSession outcome", async ({ forceStop, expected }) => {
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: enabledDescriptorForProvider,
      stopQuiesceTimeoutMs: 1,
      stopForceTimeoutMs: 1,
      startRun: () => ({
        result: new Promise(() => {}),
        interrupt: () => {},
        ...(forceStop ? { forceStop } : {}),
      }),
    });
    await handle(
      command("StartSession", { runnerInput: remoteRunnerInput() }, `session-${expected}`),
    );
    void handle(command("SendTurn", { inputText: "hang" }, `turn-${expected}`));

    const stop = await handle(command("StopSession", {}, `stop-${expected}`));

    expect(stop.at(-1)).toMatchObject({
      messageType: "Result",
      payload: { outcome: expected, quiesced: false, graceful: false },
    });
  });

  it("reports forced only after forced teardown reaches the operation terminal", async () => {
    let rejectRun: ((error: Error) => void) | undefined;
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      descriptorForProvider: enabledDescriptorForProvider,
      stopQuiesceTimeoutMs: 1,
      stopForceTimeoutMs: 25,
      startRun: () => ({
        result: new Promise((_, reject) => {
          rejectRun = reject;
        }),
        interrupt: () => {},
        forceStop: () => rejectRun?.(new ProviderInterruptedError()),
      }),
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-forced"));
    const turn = handle(command("SendTurn", { inputText: "hang" }, "turn-forced"));

    const stop = await handle(command("StopSession", {}, "stop-forced"));

    await turn;
    expect(stop.at(-1)).toMatchObject({
      messageType: "Result",
      payload: { outcome: "forced", quiesced: false, graceful: false },
    });
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

  it("rejects SuspendTurn for an active primary operation", async () => {
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
      command("ResumeSession", { runnerInput: remoteRunnerInput(true) }, "session-primary-suspend"),
    );

    const compact = handle(command("CompactSession", {}, "compact-suspend"));
    const suspend = await handle(
      command("SuspendTurn", { targetCommandId: "compact-suspend" }, "suspend-primary"),
    );

    expect(suspend.at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "capability_unsupported" },
    });

    rejectRun?.(new Error("Provider operation was interrupted."));
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
      startRun: () => ({
        result: Promise.resolve({
          type: "result",
          output: { provider: "claudeAgent", text: "ok" },
        }),
        interrupt: () => undefined,
      }),
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
          eventVersion: CLOUD_AGENT_RUNTIME_EVENT_VERSION,
          eventType: "content.delta",
          payload: { streamKind: "assistant_text", delta: "canonical" },
        },
      }),
    );
  });

  it("streams intermediate Turn events without retaining them in the handler result", async () => {
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
      descriptorForProvider: enabledDescriptorForProvider,
      startRun: (_input, _credential, emit) => {
        for (let index = 0; index < 200; index += 1) {
          emit({
            type: "event",
            eventType: "runtime.output.delta",
            payload: { text: `chunk-${index}` },
          });
        }
        return {
          result: Promise.resolve({ type: "result", output: { text: "done" } }),
          interrupt: () => {},
        } satisfies ProviderRunController;
      },
    });
    await handle(
      command("StartSession", { runnerInput: remoteRunnerInput() }, "session-streaming"),
    );
    emitted.length = 0;

    const result = await handle(command("SendTurn", { inputText: "stream" }, "send-streaming"));

    expect(result).toHaveLength(1);
    expect(result[0]).toMatchObject({ commandId: "send-streaming", messageType: "Result" });
    expect(emitted).toHaveLength(201);
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

  it("flushes the output sink before the protocol run resolves", async () => {
    const source = new PassThrough();
    const emitted: ProviderHostMessageEnvelope[] = [];
    let flushedMessageCount = -1;
    const protocol = runProviderHostProtocolV2({
      source,
      credential: null,
      emit: (message) => emitted.push(message),
      flush: async () => {
        await Promise.resolve();
        flushedMessageCount = emitted.length;
      },
      descriptorForProvider: enabledDescriptorForProvider,
    });

    source.end(`${JSON.stringify(command("Describe", { provider: "codex" }, "describe-flush"))}\n`);
    await protocol;

    expect(flushedMessageCount).toBe(1);
    expect(emitted[0]).toMatchObject({ commandId: "describe-flush", messageType: "Result" });
  });
});

function compatibleCodexProbe(): ProviderVersionProbeResult {
  return { available: true, output: "codex-cli 0.145.0" };
}

function enabledDescriptorForProvider(provider: ProviderHostProviderKind) {
  return providerHostDescriptor(provider, {
    environment: {
      SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "codex,claudeAgent",
    },
    runtimeVersionProbe: compatibleCodexProbe,
    runtimeVersion: "0.3.207",
  });
}

function codexDescriptorFactory(probe: ProviderVersionProbeResult) {
  return (provider: ProviderHostProviderKind) =>
    providerHostDescriptor(provider, {
      environment: { SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "codex" },
      runtimeVersionProbe: () => probe,
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
