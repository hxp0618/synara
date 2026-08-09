import { Readable, Writable } from "node:stream";

import { CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION } from "@synara/cloud-agent-provider-api";
import { describe, expect, it } from "vitest";

import { createCloudAgentRuntime } from "./runtime";
import { runCloudAgentRuntimeStdio } from "./runtimeStdio";

const describeCommand = {
  requestId: "request-describe",
  protocolVersion: { major: 2, minor: 3 },
  executionId: "execution-describe",
  generation: 1,
  commandType: "Describe" as const,
  commandId: "command-describe",
  occurredAt: "2026-08-09T00:00:00.000Z",
  payload: { provider: "codex" },
};

const startCommand = {
  ...describeCommand,
  requestId: "request-start",
  executionId: "execution-start",
  commandType: "StartSession" as const,
  commandId: "command-start",
  payload: {
    runnerInput: {
      execution: { id: "thread-start" },
      workload: { provider: "codex", inputText: "" },
      workspaceDirectory: "/tmp/cloud-agent-runtime-stdio-test",
    },
  },
};

const sendCommand = {
  ...startCommand,
  requestId: "request-send",
  commandType: "SendTurn" as const,
  commandId: "command-send",
  payload: { inputText: "hello" },
};

describe("runCloudAgentRuntimeStdio", () => {
  it("routes Describe through the explicit Plugin registry", async () => {
    const output = captureOutput();
    await runCloudAgentRuntimeStdio(fakeRuntime(), {
      source: Readable.from([`${JSON.stringify(describeCommand)}\n`]),
      output: output.stream,
      allowedProviders: ["codex"],
    });

    expect(JSON.parse(output.text())).toMatchObject({
      messageType: "Result",
      commandId: "command-describe",
      payload: { descriptor: { providerKind: "codex", displayName: "Codex" } },
    });
  });

  it("fails closed when a registered Provider is absent from the runtime allowlist", async () => {
    const output = captureOutput();
    await runCloudAgentRuntimeStdio(fakeRuntime(), {
      source: Readable.from([`${JSON.stringify(describeCommand)}\n`]),
      output: output.stream,
      allowedProviders: [],
    });

    expect(JSON.parse(output.text())).toMatchObject({
      messageType: "Error",
      error: { code: "provider_not_installed", message: "Provider codex is disabled." },
    });
  });

  it("bounds raw stdin before a newline is received", async () => {
    const output = captureOutput();
    await expect(
      runCloudAgentRuntimeStdio(fakeRuntime(), {
        source: Readable.from([Buffer.alloc(2 * 1024 * 1024 + 1, 0x78)]),
        output: output.stream,
      }),
    ).rejects.toThrow("exceeds the negotiated size limit");
    expect(output.text()).toBe("");
  });

  it("fails closed on invalid UTF-8 instead of accepting replacement characters", async () => {
    const output = captureOutput();
    await expect(
      runCloudAgentRuntimeStdio(fakeRuntime(), {
        source: Readable.from([Buffer.from([0x7b, 0xc3, 0x28, 0x7d, 0x0a])]),
        output: output.stream,
      }),
    ).rejects.toThrow("not valid UTF-8");
    expect(output.text()).toBe("");
  });

  it("terminates the stream after malformed input instead of processing later commands", async () => {
    const output = captureOutput();
    await expect(
      runCloudAgentRuntimeStdio(fakeRuntime(), {
        source: Readable.from([`{malformed}\n${JSON.stringify(describeCommand)}\n`]),
        output: output.stream,
      }),
    ).rejects.toThrow();
    expect(output.text()).toBe("");
  });

  it("propagates a rejecting event stream immediately and closes its session", async () => {
    const eventFailure = new Error("Provider event stream failed.");
    const unhandled: unknown[] = [];
    const onUnhandled = (cause: unknown) => unhandled.push(cause);
    process.on("unhandledRejection", onUnhandled);
    const source = new Readable({ read() {} });
    source.push(`${JSON.stringify(startCommand)}\n`);
    let describeCalls = 0;
    let closes = 0;
    const session = {
      sessionId: "session-rejecting-events",
      events: {
        [Symbol.asyncIterator]() {
          return {
            async next(): Promise<IteratorResult<never>> {
              throw eventFailure;
            },
          };
        },
      },
      async execute(command: typeof startCommand) {
        return terminalForCommand(command);
      },
      async close() {
        closes += 1;
      },
      async [Symbol.asyncDispose]() {
        await this.close();
      },
    };
    const runtime = createCloudAgentRuntime({
      providers: [
        {
          ...testProviderDescriptor(),
          async describe() {
            describeCalls += 1;
            return await fakeRuntime().describe("codex");
          },
          async createSession() {
            return session;
          },
        },
      ],
    });

    try {
      const running = runCloudAgentRuntimeStdio(runtime, {
        source,
        output: captureOutput().stream,
      });
      setImmediate(() => {
        if (!source.destroyed) source.push(`${JSON.stringify(describeCommand)}\n`);
      });

      await expect(running).rejects.toBe(eventFailure);
      await new Promise<void>((resolve) => setImmediate(resolve));
      expect(source.destroyed).toBe(true);
      expect(describeCalls).toBe(0);
      expect(closes).toBe(1);
      expect(unhandled).toEqual([]);
    } finally {
      process.off("unhandledRejection", onUnhandled);
    }
  });

  it("propagates the original failure from a backpressured output and closes sessions", async () => {
    const outputFailure = new Error("Provider Host stdout write failed.");
    const unhandled: unknown[] = [];
    const onUnhandled = (cause: unknown) => unhandled.push(cause);
    process.on("unhandledRejection", onUnhandled);
    const source = new Readable({ read() {} });
    source.push(`${JSON.stringify(startCommand)}\n`);
    let closes = 0;
    let eventsClosed: (() => void) | undefined;
    const closed = new Promise<void>((resolve) => {
      eventsClosed = resolve;
    });
    const progress = {
      ...terminalForCommand(startCommand),
      messageType: "Progress" as const,
      payload: { phase: "started" },
    };
    let emitted = false;
    const closeFailingOutputSession = async () => {
      closes += 1;
      eventsClosed?.();
    };
    const runtime = runtimeWithProvider(async () => ({
      sessionId: "session-failing-output",
      events: {
        [Symbol.asyncIterator]() {
          return {
            async next() {
              if (!emitted) {
                emitted = true;
                return { value: progress, done: false } as const;
              }
              await closed;
              return { value: undefined, done: true } as const;
            },
          };
        },
      },
      async execute(command) {
        return terminalForCommand(command);
      },
      close: closeFailingOutputSession,
      async [Symbol.asyncDispose]() {
        await closeFailingOutputSession();
      },
    }));
    const output = new Writable({
      highWaterMark: 1,
      write(_chunk, _encoding, callback) {
        setImmediate(() => callback(outputFailure));
      },
    });

    try {
      await expect(runCloudAgentRuntimeStdio(runtime, { source, output })).rejects.toBe(
        outputFailure,
      );
      await new Promise<void>((resolve) => setImmediate(resolve));
      expect(source.destroyed).toBe(true);
      expect(closes).toBe(1);
      expect(unhandled).toEqual([]);
    } finally {
      process.off("unhandledRejection", onUnhandled);
    }
  });

  it("makes a delayed StartSession the admission barrier for a back-to-back SendTurn", async () => {
    let allowCreate: (() => void) | undefined;
    const createAllowed = new Promise<void>((resolve) => {
      allowCreate = resolve;
    });
    const executed: string[] = [];
    let closes = 0;
    let signalSend: (() => void) | undefined;
    const sendSeen = new Promise<void>((resolve) => {
      signalSend = resolve;
    });
    let endSource: (() => void) | undefined;
    const sourceEnded = new Promise<void>((resolve) => {
      endSource = resolve;
    });
    const runtime = runtimeWithProvider(async () => {
      await createAllowed;
      return testSession(
        "delayed",
        executed,
        () => {
          closes += 1;
        },
        (commandType) => {
          if (commandType === "SendTurn") signalSend?.();
        },
      );
    });
    async function* source() {
      yield `${JSON.stringify(startCommand)}\n${JSON.stringify(sendCommand)}\n`;
      await sourceEnded;
    }
    const running = runCloudAgentRuntimeStdio(runtime, {
      source: Readable.from(source()),
      output: captureOutput().stream,
    });

    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(executed).toEqual([]);
    allowCreate?.();
    await sendSeen;
    endSource?.();
    await running;
    expect(executed).toEqual(["StartSession", "SendTurn"]);
    expect(closes).toBe(1);
  });

  it("serializes two StartSession replacements without leaking either session", async () => {
    const executed: string[] = [];
    const closed: string[] = [];
    let createsInFlight = 0;
    let maximumCreatesInFlight = 0;
    let sessionNumber = 0;
    let signalSecondStart: (() => void) | undefined;
    const secondStartSeen = new Promise<void>((resolve) => {
      signalSecondStart = resolve;
    });
    let endSource: (() => void) | undefined;
    const sourceEnded = new Promise<void>((resolve) => {
      endSource = resolve;
    });
    const runtime = runtimeWithProvider(async () => {
      createsInFlight += 1;
      maximumCreatesInFlight = Math.max(maximumCreatesInFlight, createsInFlight);
      await Promise.resolve();
      createsInFlight -= 1;
      sessionNumber += 1;
      const id = `session-${sessionNumber}`;
      return testSession(
        id,
        executed,
        () => closed.push(id),
        (commandType) => {
          if (id === "session-2" && commandType === "StartSession") signalSecondStart?.();
        },
      );
    });
    const replacement = {
      ...startCommand,
      requestId: "request-start-2",
      commandId: "command-start-2",
      generation: 2,
    };

    async function* source() {
      yield `${JSON.stringify(startCommand)}\n${JSON.stringify(replacement)}\n`;
      await sourceEnded;
    }
    const running = runCloudAgentRuntimeStdio(runtime, {
      source: Readable.from(source()),
      output: captureOutput().stream,
    });
    await secondStartSeen;
    endSource?.();
    await running;

    expect(maximumCreatesInFlight).toBe(1);
    expect(executed).toEqual(["StartSession", "StartSession"]);
    expect(closed).toEqual(["session-1", "session-2"]);
  });

  it("closes a session whose delayed creation completes after fatal input", async () => {
    let allowCreate: (() => void) | undefined;
    const createAllowed = new Promise<void>((resolve) => {
      allowCreate = resolve;
    });
    const executed: string[] = [];
    let closes = 0;
    let signalLateClose: (() => void) | undefined;
    const lateClosed = new Promise<void>((resolve) => {
      signalLateClose = resolve;
    });
    const runtime = runtimeWithProvider(async () => {
      await createAllowed;
      return testSession("late", executed, () => {
        closes += 1;
        signalLateClose?.();
      });
    });
    async function* source() {
      yield `${JSON.stringify(startCommand)}\n`;
      await new Promise<void>((resolve) => setImmediate(resolve));
      yield "{malformed}\n";
    }
    let signalFatal: (() => void) | undefined;
    const fatalSeen = new Promise<void>((resolve) => {
      signalFatal = resolve;
    });
    const diagnostics = new Writable({
      write(_chunk, _encoding, callback) {
        signalFatal?.();
        callback();
      },
    });
    const running = runCloudAgentRuntimeStdio(runtime, {
      source: Readable.from(source()),
      output: captureOutput().stream,
      diagnostics,
    });

    await fatalSeen;
    await expect(running).rejects.toThrow();
    allowCreate?.();
    await lateClosed;
    expect(executed).toEqual([]);
    expect(closes).toBe(1);
  });

  it("binds credential acquisition and consumes the inherited FD only once", async () => {
    const validationFailures: string[] = [];
    let sessionNumber = 0;
    const runtime = createCloudAgentRuntime({
      providers: [
        {
          ...testProviderDescriptor(),
          async createSession(_input, host) {
            if (sessionNumber === 0) {
              for (const request of [
                { providerKind: "other", operation: "session", generation: 1 },
                { providerKind: "codex", operation: "turn", generation: 1 },
                { providerKind: "codex", operation: "session", generation: 2 },
              ]) {
                try {
                  await host.credential.acquire(request);
                } catch (cause) {
                  validationFailures.push(cause instanceof Error ? cause.message : String(cause));
                }
              }
            }
            try {
              await host.credential.acquire({
                providerKind: "codex",
                operation: "session",
                generation: host.workspace.generation,
              });
            } catch (cause) {
              validationFailures.push(cause instanceof Error ? cause.message : String(cause));
              throw cause;
            }
            sessionNumber += 1;
            return testSession(`credential-${sessionNumber}`, [], () => undefined);
          },
        },
      ],
    });
    const replacement = {
      ...startCommand,
      requestId: "request-credential-2",
      commandId: "command-credential-2",
      generation: 2,
    };
    const output = captureOutput();
    let endSource: (() => void) | undefined;
    const sourceEnded = new Promise<void>((resolve) => {
      endSource = resolve;
    });
    async function* source() {
      yield `${JSON.stringify(startCommand)}\n${JSON.stringify(replacement)}\n`;
      await sourceEnded;
    }
    const running = runCloudAgentRuntimeStdio(runtime, {
      source: Readable.from(source()),
      output: output.stream,
      environment: { SYNARA_PROVIDER_CREDENTIAL_FD: "3" },
    });
    while (validationFailures.length < 4) {
      await new Promise<void>((resolve) => setImmediate(resolve));
    }
    endSource?.();
    await running;

    expect(validationFailures).toEqual([
      "Credential request does not match the active Provider session binding.",
      "Credential request does not match the active Provider session binding.",
      "Credential request does not match the active Provider session binding.",
      "Runtime credential descriptor has already been consumed.",
    ]);
    expect(output.text()).toContain("Runtime credential descriptor has already been consumed.");
  });

  it("closes an active Provider before waiting for a command blocked on execute", async () => {
    let closeCalled = false;
    let resolveExecute:
      | ((message: {
          messageType: "Result";
          requestId: string;
          protocolVersion: { major: number; minor: number };
          executionId: string;
          generation: number;
          commandId: string;
          occurredAt: string;
          payload: Record<string, unknown>;
        }) => void)
      | undefined;
    let resolveEvents: (() => void) | undefined;
    const eventsClosed = new Promise<void>((resolve) => {
      resolveEvents = resolve;
    });
    const runtime = createCloudAgentRuntime({
      providers: [
        {
          abiVersion: CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
          providerKind: "codex",
          async describe() {
            return await fakeRuntime().describe("codex");
          },
          async createSession() {
            const closeSession = async () => {
              closeCalled = true;
              resolveExecute?.({
                requestId: startCommand.requestId,
                protocolVersion: startCommand.protocolVersion,
                executionId: startCommand.executionId,
                generation: startCommand.generation,
                commandId: startCommand.commandId,
                occurredAt: startCommand.occurredAt,
                messageType: "Result",
                payload: { closed: true },
              });
              resolveEvents?.();
            };
            return {
              sessionId: "session-blocked",
              events: {
                async *[Symbol.asyncIterator]() {
                  await eventsClosed;
                  yield* [];
                },
              },
              execute(command) {
                return new Promise((resolve) => {
                  resolveExecute = () =>
                    resolve({
                      requestId: command.requestId,
                      protocolVersion: command.protocolVersion,
                      executionId: command.executionId,
                      generation: command.generation,
                      commandId: command.commandId,
                      occurredAt: command.occurredAt,
                      messageType: "Result",
                      payload: { closed: true },
                    });
                });
              },
              close: closeSession,
              async [Symbol.asyncDispose]() {
                await closeSession();
              },
            };
          },
        },
      ],
    });
    async function* source() {
      yield `${JSON.stringify(startCommand)}\n`;
      await new Promise<void>((resolve) => setImmediate(resolve));
      yield "{malformed}\n";
    }

    await expect(
      runCloudAgentRuntimeStdio(runtime, {
        source: Readable.from(source()),
        output: captureOutput().stream,
        diagnostics: captureOutput().stream,
      }),
    ).rejects.toThrow();
    expect(closeCalled).toBe(true);
  });
});

function fakeRuntime() {
  return createCloudAgentRuntime({
    providers: [
      {
        abiVersion: CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
        providerKind: "codex",
        async describe() {
          return {
            abiVersion: CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
            providerKind: "codex",
            displayName: "Codex",
            adapterVersion: "test",
            runtime: {
              kind: "cli" as const,
              name: "codex",
              available: true,
              compatible: true,
              compatibleRange: { minimumInclusive: "0.0.0" },
            },
            capabilities: {} as never,
          };
        },
        async createSession() {
          throw new Error("Describe must not create a Provider Session.");
        },
      },
    ],
  });
}

function runtimeWithProvider(
  createSession: Parameters<
    typeof createCloudAgentRuntime
  >[0]["providers"][number]["createSession"],
) {
  return createCloudAgentRuntime({ providers: [{ ...testProviderDescriptor(), createSession }] });
}

function testProviderDescriptor() {
  return {
    abiVersion: CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
    providerKind: "codex",
    async describe() {
      return await fakeRuntime().describe("codex");
    },
  } as const;
}

function testSession(
  id: string,
  executed: string[],
  onClose: () => void,
  onExecute: (commandType: string) => void = () => undefined,
) {
  let closeEvents: (() => void) | undefined;
  const eventsClosed = new Promise<void>((resolve) => {
    closeEvents = resolve;
  });
  let closed = false;
  const close = async () => {
    if (closed) return;
    closed = true;
    onClose();
    closeEvents?.();
  };
  return {
    sessionId: id,
    events: {
      async *[Symbol.asyncIterator]() {
        await eventsClosed;
        yield* [];
      },
    },
    async execute(command: typeof startCommand | typeof sendCommand) {
      executed.push(command.commandType);
      onExecute(command.commandType);
      return {
        requestId: command.requestId,
        protocolVersion: command.protocolVersion,
        executionId: command.executionId,
        generation: command.generation,
        commandId: command.commandId,
        occurredAt: command.occurredAt,
        messageType: "Result" as const,
        payload: {},
      };
    },
    close,
    async [Symbol.asyncDispose]() {
      await close();
    },
  };
}

function terminalForCommand(command: {
  readonly requestId: string;
  readonly protocolVersion: { readonly major: number; readonly minor: number };
  readonly executionId: string;
  readonly generation: number;
  readonly commandId: string;
  readonly occurredAt: string;
}) {
  return {
    requestId: command.requestId,
    protocolVersion: command.protocolVersion,
    executionId: command.executionId,
    generation: command.generation,
    commandId: command.commandId,
    occurredAt: command.occurredAt,
    messageType: "Result" as const,
    payload: {},
  };
}

function captureOutput(): { stream: Writable; text: () => string } {
  let value = "";
  return {
    stream: new Writable({
      write(chunk, _encoding, callback) {
        value += chunk.toString();
        callback();
      },
    }),
    text: () => value.trim(),
  };
}
