import { closeSync, mkdtempSync, openSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import {
  CLOUD_AGENT_PROTOCOL_VERSION,
  type CloudAgentCommandEnvelope,
} from "@synara/cloud-agent-protocol";
import type {
  CloudAgentCredentialLease,
  CloudAgentHostServices,
} from "@synara/cloud-agent-provider-api";

import { createProviderPlugin, type ProviderPluginOptions } from "./providerPlugin";
import type {
  ProviderRunController,
  ProviderRunExecutor,
  RunnerInput,
  RunnerMessage,
} from "./internalExecution";

const host: CloudAgentHostServices = {
  workspace: {
    authority: "host",
    root: "/tmp/cloud-agent-plugin-test",
    generation: 1,
    readOnly: false,
  },
  credential: { acquire: async () => null },
  log: {
    debug: () => undefined,
    info: () => undefined,
    warn: () => undefined,
    error: () => undefined,
  },
};

describe("createProviderPlugin", () => {
  it("adapts Describe through the app-neutral Provider Plugin ABI", async () => {
    const plugin = createProviderPlugin({
      providerKind: "codex",
      displayName: "Codex",
      startRun: unavailableExecutor,
    });
    const session = await createSession(plugin, host);
    const terminal = await session.execute(command("Describe", { provider: "codex" }));

    expect(terminal.messageType).toBe("Result");
    if (terminal.messageType === "Result") {
      expect(terminal.payload.descriptor).toMatchObject({
        capabilityDescriptor: { provider: "codex" },
      });
    }
    await session.close();
  });

  it("does not let one provider package execute another provider kind", async () => {
    const plugin = createProviderPlugin({
      providerKind: "codex",
      displayName: "Codex",
      startRun: unavailableExecutor,
    });
    const session = await createSession(plugin, host);
    await expect(
      session.execute(command("Describe", { provider: "claudeAgent" }, "command-other")),
    ).rejects.toThrow("cannot execute provider claudeAgent");
    await session.close();
  });

  it("releases the credential lease when session setup fails", async () => {
    let disposals = 0;
    const invalidFd = openSync("/dev/null", "r");
    closeSync(invalidFd);
    const lease: CloudAgentCredentialLease = {
      delivery: "anonymous-fd",
      fd: invalidFd,
      async [Symbol.asyncDispose]() {
        disposals += 1;
      },
    };
    const plugin = createProviderPlugin({
      providerKind: "codex",
      displayName: "Codex",
      startRun: unavailableExecutor,
    });

    await expect(
      createSession(plugin, {
        ...host,
        credential: { acquire: async () => lease },
      }),
    ).rejects.toThrow();
    expect(disposals).toBe(1);
  });

  it("shares close work and waits for artifact acceptance before releasing credentials", async () => {
    const credential = testCredentialLease();
    let acceptArtifact: (() => void) | undefined;
    const artifactAccepted = new Promise<void>((resolve) => {
      acceptArtifact = resolve;
    });
    let disposals = 0;
    const plugin = testPlugin({
      startRun: (_input, _credential, emit) => {
        emit({
          type: "artifact",
          artifact: {
            path: "report.txt",
            kind: "generated-file",
            contentType: "text/plain",
            sourceRoot: "workspace",
          },
        });
        return completedRun("artifact ready");
      },
    });

    try {
      const session = await createSession(plugin, {
        ...host,
        credential: {
          acquire: async () => ({
            ...credential.lease,
            async [Symbol.asyncDispose]() {
              disposals += 1;
            },
          }),
        },
        acceptArtifact: async () => artifactAccepted,
      });
      await session.execute(command("StartSession", { runnerInput: runnerInput() }, "start"));
      await session.execute(command("SendTurn", { inputText: "write report" }, "send"));

      const firstClose = session.close("test");
      const secondClose = session.close("duplicate");
      expect(firstClose).toBe(secondClose);
      await Promise.resolve();
      expect(disposals).toBe(0);

      acceptArtifact?.();
      await firstClose;
      expect(disposals).toBe(1);
    } finally {
      credential.cleanup();
    }
  });

  it("interrupts an aborted primary command and suppresses its late output", async () => {
    let rejectRun: ((cause: Error) => void) | undefined;
    let emitRun: ((message: RunnerMessage) => void) | undefined;
    let interrupts = 0;
    let acceptedArtifacts = 0;
    const warnings: string[] = [];
    const plugin = testPlugin({
      startRun: (_input, _credential, emit) => {
        emitRun = emit;
        return {
          result: new Promise((_, reject) => {
            rejectRun = reject;
          }),
          interrupt: () => {
            interrupts += 1;
            emitRun?.({
              type: "artifact",
              artifact: {
                path: "late.txt",
                kind: "generated-file",
                contentType: "text/plain",
                sourceRoot: "workspace",
              },
            });
            rejectRun?.(new Error("Provider operation was interrupted."));
          },
        } satisfies ProviderRunController;
      },
    });
    const session = await createSession(plugin, {
      ...host,
      log: {
        ...host.log,
        warn: (message) => warnings.push(message),
      },
      acceptArtifact: async () => {
        acceptedArtifacts += 1;
      },
    });
    await session.execute(command("StartSession", { runnerInput: runnerInput() }, "start-abort"));
    const iterator = session.events[Symbol.asyncIterator]();
    await iterator.next();

    const controller = new AbortController();
    const execution = session.execute(
      command("SendTurn", { inputText: "long operation" }, "send-abort"),
      controller.signal,
    );
    controller.abort();

    await expect(execution).rejects.toMatchObject({ name: "AbortError" });
    await session.close();
    expect(interrupts).toBe(1);
    expect(acceptedArtifacts).toBe(0);
    expect(warnings).toContain(
      "Cloud Agent command was aborted; subsequent messages are suppressed until it settles.",
    );
    await expect(iterator.next()).resolves.toEqual({ value: undefined, done: true });
  });

  it("suppresses an aborted non-cancellable command and makes close await convergence", async () => {
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    const warnings: string[] = [];
    const plugin = testPlugin({
      startRun: () => ({
        result: new Promise((resolve) => {
          completeRun = resolve;
        }),
        interrupt: () => undefined,
      }),
    });
    const session = await createSession(plugin, {
      ...host,
      log: { ...host.log, warn: (message) => warnings.push(message) },
    });
    await session.execute(
      command("StartSession", { runnerInput: runnerInput() }, "start-generate"),
    );
    const iterator = session.events[Symbol.asyncIterator]();
    await iterator.next();

    const controller = new AbortController();
    const generation = session.execute(
      command(
        "GenerateText",
        { task: "thread-title", input: { message: "long input" } },
        "generate-abort",
      ),
      controller.signal,
    );
    controller.abort();
    await expect(generation).rejects.toMatchObject({ name: "AbortError" });

    let closed = false;
    const close = session.close().then(() => {
      closed = true;
    });
    await Promise.resolve();
    expect(closed).toBe(false);
    expect(warnings).toContain(
      "Cloud Agent command has no direct cancellation primitive; close will wait for it to settle.",
    );

    completeRun?.({
      type: "result",
      output: { text: '{"task":"thread-title","title":"settled"}' },
    });
    await close;
    await expect(iterator.next()).resolves.toEqual({ value: undefined, done: true });
  });

  it("bounds close when an interrupted Provider command never settles", async () => {
    const warnings: string[] = [];
    const plugin = testPlugin({
      startRun: () => ({
        result: new Promise(() => undefined),
        interrupt: () => undefined,
      }),
      stopQuiesceTimeoutMs: 5,
      stopForceTimeoutMs: 5,
      closeTaskTimeoutMs: 5,
    });
    const session = await createSession(plugin, {
      ...host,
      log: { ...host.log, warn: (message) => warnings.push(message) },
    });
    await session.execute(command("StartSession", { runnerInput: runnerInput() }, "start-hung"));
    const execution = session.execute(
      command("SendTurn", { inputText: "never settles" }, "send-hung"),
    );
    void execution.catch(() => undefined);

    const firstClose = session.close("bounded");
    expect(session.close("shared")).toBe(firstClose);
    await firstClose;

    expect(warnings).toContain(
      "Cloud Agent Provider close timed out waiting for background tasks; late output was suppressed.",
    );
  });

  it("aborts and bounds a hung Artifact acceptance before releasing the credential lease", async () => {
    const credential = testCredentialLease();
    const warnings: string[] = [];
    let artifactAborted = false;
    let disposals = 0;
    const plugin = testPlugin({
      startRun: (_input, _credential, emit) => {
        emit({
          type: "artifact",
          artifact: {
            path: "hung.txt",
            kind: "generated-file",
            contentType: "text/plain",
            sourceRoot: "workspace",
          },
        });
        return completedRun("artifact emitted");
      },
      closeTaskTimeoutMs: 5,
    });
    try {
      const session = await createSession(plugin, {
        ...host,
        credential: {
          acquire: async () => ({
            ...credential.lease,
            async [Symbol.asyncDispose]() {
              disposals += 1;
            },
          }),
        },
        log: { ...host.log, warn: (message) => warnings.push(message) },
        acceptArtifact: async (_artifact, signal) => {
          signal?.addEventListener("abort", () => {
            artifactAborted = true;
          });
          return await new Promise<void>(() => undefined);
        },
      });
      await session.execute(
        command("StartSession", { runnerInput: runnerInput() }, "start-hung-artifact"),
      );
      await session.execute(
        command("SendTurn", { inputText: "emit artifact" }, "send-hung-artifact"),
      );

      await session.close();
      expect(artifactAborted).toBe(true);
      expect(disposals).toBe(1);
      expect(warnings).toContain(
        "Cloud Agent Provider close timed out waiting for background tasks; late output was suppressed.",
      );
    } finally {
      credential.cleanup();
    }
  });

  it("preserves the terminal receipt when progress exceeds the bounded event queue", async () => {
    const warnings: string[] = [];
    const plugin = testPlugin({
      startRun: (_input, _credential, emit) => {
        for (let index = 0; index < 2_050; index += 1) {
          emit({
            type: "artifact",
            artifact: {
              path: `generated-${index}.txt`,
              kind: "generated-file",
              contentType: "text/plain",
              sourceRoot: "workspace",
            },
          });
        }
        return completedRun("complete");
      },
    });
    const session = await createSession(plugin, {
      ...host,
      log: { ...host.log, warn: (message) => warnings.push(message) },
    });
    await session.execute(
      command("StartSession", { runnerInput: runnerInput() }, "start-overflow"),
    );
    const iterator = session.events[Symbol.asyncIterator]();
    await iterator.next();
    await session.execute(command("SendTurn", { inputText: "many events" }, "send-overflow"));

    const buffered = await Promise.all(
      Array.from({ length: 2_048 }, async () => (await iterator.next()).value),
    );
    expect(buffered.at(-1)).toMatchObject({
      commandId: "send-overflow",
      messageType: "Result",
    });
    expect(warnings).toEqual([
      "Cloud Agent event queue reached its bounded capacity; oldest progress was discarded.",
    ]);
    await session.close();
  });

  it("binds command generation, Workspace, and model to Host authority", async () => {
    let observedInput: RunnerInput | undefined;
    const plugin = testPlugin({
      startRun: (input) => {
        observedInput = input;
        return completedRun("done");
      },
    });
    const session = await createSession(plugin, host, { model: "gpt-host-pinned" });
    await session.execute(command("StartSession", { runnerInput: runnerInput() }, "start-bound"));
    await session.execute(command("SendTurn", { inputText: "hello" }, "send-bound"));

    expect(observedInput?.workload.model).toBe("gpt-host-pinned");
    await expect(
      session.execute({
        ...command("StopSession", {}, "wrong-generation"),
        generation: 2,
      }),
    ).rejects.toThrow("does not match Host generation");
    await expect(
      session.execute(
        command(
          "StartSession",
          { runnerInput: { ...runnerInput(), workspaceDirectory: "/tmp/other-workspace" } },
          "wrong-workspace",
        ),
      ),
    ).rejects.toThrow("does not match Host authority");
    await session.close();
  });

  it("rejects path traversal artifacts and preserves validated digest metadata", async () => {
    const accepted: Array<{ relativePath: string; sha256?: string }> = [];
    const warnings: string[] = [];
    const plugin = testPlugin({
      startRun: (_input, _credential, emit) => {
        emit({
          type: "artifact",
          artifact: {
            path: "../escape.txt",
            kind: "generated_file",
            contentType: "text/plain",
            sourceRoot: "workspace",
          },
        });
        emit({
          type: "artifact",
          artifact: {
            path: "reports/result.txt",
            kind: "generated_file",
            contentType: "text/plain",
            sourceRoot: "workspace",
            sha256: "A".repeat(64),
            reportedSize: 12,
          },
        });
        return completedRun("artifacts ready");
      },
    });
    const session = await createSession(plugin, {
      ...host,
      log: { ...host.log, warn: (message) => warnings.push(message) },
      acceptArtifact: async (artifact) => {
        accepted.push(artifact);
      },
    });
    await session.execute(command("StartSession", { runnerInput: runnerInput() }, "start-path"));
    await session.execute(command("SendTurn", { inputText: "write" }, "send-path"));
    await session.close();

    expect(accepted).toEqual([
      expect.objectContaining({
        relativePath: "reports/result.txt",
        sha256: "a".repeat(64),
      }),
    ]);
    expect(warnings).toContain(
      "Cloud Agent emitted an invalid artifact candidate; it was ignored.",
    );
  });
});

type TestPluginOverrides = Pick<ProviderPluginOptions, "startRun"> &
  Partial<
    Pick<
      ProviderPluginOptions,
      "stopQuiesceTimeoutMs" | "stopForceTimeoutMs" | "closeTaskTimeoutMs"
    >
  >;

function testPlugin(overrides: TestPluginOverrides = { startRun: unavailableExecutor }) {
  return createProviderPlugin({
    providerKind: "codex",
    displayName: "Codex",
    descriptor: {
      environment: { SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS: "codex" },
      runtimeVersionProbe: () => ({ available: true, output: "codex-cli 0.145.0" }),
    },
    ...overrides,
  });
}

const unavailableExecutor: ProviderRunExecutor = () => {
  throw new Error("Test Provider executor is unavailable.");
};

function createSession(
  plugin: ReturnType<typeof createProviderPlugin>,
  services: CloudAgentHostServices,
  configuration: Readonly<Record<string, unknown>> = {},
) {
  return plugin.createSession(
    { hostInstanceId: "instance-1", hostThreadId: "thread-1", configuration },
    services,
  );
}

function command(
  commandType: CloudAgentCommandEnvelope["commandType"],
  payload: Record<string, unknown>,
  commandId = `command-${commandType}`,
): CloudAgentCommandEnvelope {
  return {
    requestId: `request-${commandId}`,
    protocolVersion: CLOUD_AGENT_PROTOCOL_VERSION,
    executionId: "execution-1",
    generation: 1,
    commandType,
    commandId,
    occurredAt: "2026-08-09T00:00:00.000Z",
    payload,
  } as CloudAgentCommandEnvelope;
}

function runnerInput() {
  return {
    execution: { id: "execution-1" },
    workload: { provider: "codex", inputText: "" },
    workspaceDirectory: "/tmp/cloud-agent-plugin-test",
  };
}

function completedRun(text: string): ProviderRunController {
  return {
    result: Promise.resolve({ type: "result", output: { text } }),
    interrupt: () => undefined,
  };
}

function testCredentialLease(): { lease: CloudAgentCredentialLease; cleanup: () => void } {
  const directory = mkdtempSync(join(tmpdir(), "cloud-agent-plugin-credential-"));
  const path = join(directory, "credential.json");
  writeFileSync(path, '{"payload":{"token":"opaque-test-value"}}');
  const fd = openSync(path, "r");
  return {
    lease: {
      delivery: "anonymous-fd",
      fd,
      async [Symbol.asyncDispose]() {},
    },
    cleanup: () => {
      closeSync(fd);
      rmSync(directory, { recursive: true, force: true });
    },
  };
}
