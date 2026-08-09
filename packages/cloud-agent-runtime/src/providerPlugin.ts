import { resolve } from "node:path";

import type {
  CloudAgentCommandEnvelope,
  CloudAgentMessageEnvelope,
} from "@synara/cloud-agent-protocol";
import {
  CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
  type CloudAgentHostServices,
  type CloudAgentProviderDescriptor,
  type CloudAgentProviderPluginV1,
  type CloudAgentProviderSession,
} from "@synara/cloud-agent-provider-api";
import type { ProviderHostCommandEnvelope } from "@synara/contracts/provider-host";

import {
  createProviderHostProtocolHandler,
  providerHostDescriptor,
  type ProviderHostDescriptorOptions,
} from "./protocol";
import { readRunnerCredential, startProviderHostRun, type RunnerInput } from "./providerHost";

export type PortableProviderKind = "codex" | "claudeAgent";

export interface LegacyProviderPluginOptions {
  readonly providerKind: PortableProviderKind;
  readonly displayName: string;
  readonly descriptor?: ProviderHostDescriptorOptions;
  readonly configurationSchema?: Readonly<Record<string, unknown>>;
  /** @internal Provider execution seam retained for adapter-level conformance tests. */
  readonly startRun?: typeof startProviderHostRun;
}

/**
 * Compatibility adapter used while the Provider Host v2 implementation is
 * moved out of the historical Synara package. The returned plugin exposes
 * only the app-neutral Provider Plugin ABI; legacy RunnerInput details remain
 * behind Cloud Agent command payloads and are not added to the public ABI.
 */
export function createLegacyProviderPlugin(
  options: LegacyProviderPluginOptions,
): CloudAgentProviderPluginV1 {
  const descriptor = () => providerHostDescriptor(options.providerKind, options.descriptor);
  const plugin: CloudAgentProviderPluginV1 = {
    abiVersion: CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
    providerKind: options.providerKind,
    async describe() {
      return toPluginDescriptor(options, descriptor());
    },
    async createSession(input, host, signal) {
      if (signal?.aborted) throw new Error("Cloud Agent Provider session creation was aborted.");
      const workspaceRoot = writableWorkspaceRoot(host);
      const configuredModel = readConfiguredModel(input.configuration);
      const credentialLease = await host.credential.acquire({
        providerKind: options.providerKind,
        operation: "session",
        generation: host.workspace.generation,
        ...(signal ? { signal } : {}),
      });
      let sessionCreated = false;
      try {
        if (signal?.aborted) throw new Error("Cloud Agent Provider session creation was aborted.");
        const credential = credentialLease
          ? readRunnerCredential({ SYNARA_PROVIDER_CREDENTIAL_FD: String(credentialLease.fd) })
          : null;
        let queueOverflowReported = false;
        const events = new AsyncMessageQueue<CloudAgentMessageEnvelope>(isTerminalMessage, () => {
          if (queueOverflowReported) return;
          queueOverflowReported = true;
          host.log.warn(
            "Cloud Agent event queue reached its bounded capacity; oldest progress was discarded.",
          );
        });
        const artifactTasks = new Set<Promise<void>>();
        const commandTasks = new Set<Promise<unknown>>();
        const controlTasks = new Set<Promise<unknown>>();
        const suppressedCommandIds = new Set<string>();
        let closed = false;
        let closePromise: Promise<void> | undefined;
        let lastCommand: CloudAgentCommandEnvelope | undefined;

        const handle = createProviderHostProtocolHandler({
          credential,
          emit(message) {
            if (suppressedCommandIds.has(message.commandId)) return;
            events.push(message);
            if (message.messageType === "ArtifactCandidate" && host.acceptArtifact) {
              const artifact = readArtifactCandidate(message.payload);
              if (artifact) {
                trackTask(
                  artifactTasks,
                  host.acceptArtifact(artifact).catch((cause) =>
                    host.log.warn("Cloud Agent artifact candidate was rejected by the host.", {
                      cause: cause instanceof Error ? cause.message : String(cause),
                    }),
                  ),
                );
              } else {
                host.log.warn("Cloud Agent emitted an invalid artifact candidate; it was ignored.");
              }
            }
          },
          descriptorForProvider(provider) {
            assertProvider(options.providerKind, provider);
            return descriptor();
          },
          startRun(runnerInput, runnerCredential, emit, runOptions) {
            assertRunnerProvider(options.providerKind, runnerInput);
            return (options.startRun ?? startProviderHostRun)(
              runnerInput,
              runnerCredential,
              emit,
              runOptions,
            );
          },
        });

        const execute = async (
          command: CloudAgentCommandEnvelope,
          executeSignal?: AbortSignal,
        ): Promise<CloudAgentMessageEnvelope> => {
          if (closed) throw new Error("Cloud Agent Provider session is closed.");
          if (executeSignal?.aborted) throw commandAbortError();
          const authorizedCommand = bindHostAuthority(
            command,
            host.workspace.generation,
            workspaceRoot,
            configuredModel,
          );
          assertCommandProvider(options.providerKind, authorizedCommand);
          lastCommand = authorizedCommand;
          const operation = handle(authorizedCommand as ProviderHostCommandEnvelope);
          trackTask(commandTasks, operation);
          operation.then(
            () => suppressedCommandIds.delete(command.commandId),
            () => suppressedCommandIds.delete(command.commandId),
          );
          const messages = executeSignal
            ? await raceCommandWithAbort(operation, executeSignal, () => {
                suppressedCommandIds.add(command.commandId);
                host.log.warn(
                  "Cloud Agent command was aborted; subsequent messages are suppressed until it settles.",
                  { commandId: command.commandId, commandType: command.commandType },
                );
                const interrupt = interruptCommand(command);
                if (interrupt) {
                  suppressedCommandIds.add(interrupt.commandId);
                  const interruption = handle(interrupt as ProviderHostCommandEnvelope);
                  interruption.then(
                    () => suppressedCommandIds.delete(interrupt.commandId),
                    () => suppressedCommandIds.delete(interrupt.commandId),
                  );
                  trackTask(
                    controlTasks,
                    interruption.then((interruptMessages) => {
                      const terminal = interruptMessages.at(-1);
                      if (terminal?.messageType === "Error") {
                        host.log.warn(
                          "Cloud Agent command interruption did not converge cleanly.",
                          {
                            commandId: command.commandId,
                            code: terminal.error.code,
                          },
                        );
                      }
                    }),
                  );
                } else {
                  host.log.warn(
                    "Cloud Agent command has no direct cancellation primitive; close will wait for it to settle.",
                    { commandId: command.commandId, commandType: command.commandType },
                  );
                }
              })
            : await operation;
          return terminalMessage(messages);
        };

        const close = (_reason?: string): Promise<void> => {
          if (closePromise) return closePromise;
          closed = true;
          closePromise = (async () => {
            try {
              if (lastCommand) {
                const stopCommand: CloudAgentCommandEnvelope = {
                  ...lastCommand,
                  requestId: `${lastCommand.requestId}:close`,
                  commandId: `${lastCommand.commandId}:close`,
                  commandType: "StopSession",
                  occurredAt: new Date().toISOString(),
                  payload: {},
                };
                suppressedCommandIds.add(stopCommand.commandId);
                await handle(stopCommand as ProviderHostCommandEnvelope).catch(() => undefined);
              }
              await settleTasks(commandTasks);
              await settleTasks(controlTasks);
              // Artifact validation may outlive the command terminal, but must not outlive the lease.
              await settleTasks(artifactTasks);
            } finally {
              events.close();
              await credentialLease?.[Symbol.asyncDispose]();
            }
          })();
          return closePromise;
        };

        const session: CloudAgentProviderSession = {
          sessionId: `${input.hostInstanceId}:${input.hostThreadId}`,
          events,
          execute,
          close,
          async [Symbol.asyncDispose]() {
            await close("disposed");
          },
        };
        sessionCreated = true;
        return session;
      } finally {
        if (!sessionCreated) await credentialLease?.[Symbol.asyncDispose]();
      }
    },
  };
  return Object.freeze(plugin);
}

function toPluginDescriptor(
  options: LegacyProviderPluginOptions,
  descriptor: ReturnType<typeof providerHostDescriptor>,
): CloudAgentProviderDescriptor {
  const capability = descriptor.capabilityDescriptor;
  return {
    abiVersion: CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
    providerKind: options.providerKind,
    displayName: options.displayName,
    adapterVersion: capability.adapterVersion,
    runtime: {
      kind: capability.runtime.kind,
      name: capability.runtime.name,
      ...(capability.runtime.version ? { version: capability.runtime.version } : {}),
      available: capability.runtime.available,
      compatible: capability.runtime.compatible,
      compatibleRange: {
        minimumInclusive: capability.runtime.compatibleRange.minimumInclusive,
        ...(capability.runtime.compatibleRange.maximumExclusive
          ? { maximumExclusive: capability.runtime.compatibleRange.maximumExclusive }
          : {}),
      },
    },
    capabilities: capability.capabilities,
    ...(descriptor.textGenerationTasks
      ? { textGenerationTasks: descriptor.textGenerationTasks }
      : {}),
    ...(options.configurationSchema ? { configurationSchema: options.configurationSchema } : {}),
  };
}

function assertCommandProvider(
  expected: PortableProviderKind,
  command: CloudAgentCommandEnvelope,
): void {
  if (command.commandType === "Describe") {
    assertProvider(expected, command.payload.provider);
    return;
  }
  if (command.commandType !== "StartSession" && command.commandType !== "ResumeSession") return;
  const runnerInput = command.payload.runnerInput;
  if (!isRecord(runnerInput)) throw new Error("Cloud Agent runnerInput is required.");
  assertRunnerProvider(expected, runnerInput as RunnerInput);
}

function assertRunnerProvider(expected: PortableProviderKind, runnerInput: RunnerInput): void {
  const actual = runnerInput.workload?.provider;
  assertProvider(expected, actual);
}

function writableWorkspaceRoot(host: CloudAgentHostServices): string {
  const root = host.workspace.root?.trim();
  if (!root) throw new Error("Legacy Provider compatibility requires a host-owned Workspace root.");
  if (host.workspace.authority !== "host" || host.workspace.readOnly) {
    throw new Error(
      "Legacy Provider compatibility cannot enforce an external or read-only Workspace.",
    );
  }
  return resolve(root);
}

function readConfiguredModel(configuration: Readonly<Record<string, unknown>>): string | undefined {
  const unsupported = Object.keys(configuration).filter((name) => name !== "model");
  if (unsupported.length > 0) {
    throw new Error(`Legacy Provider configuration does not support '${unsupported[0]}'.`);
  }
  if (configuration.model === undefined) return undefined;
  if (typeof configuration.model !== "string" || !configuration.model.trim()) {
    throw new Error("Legacy Provider model configuration must be a non-empty string.");
  }
  return configuration.model.trim();
}

function bindHostAuthority(
  command: CloudAgentCommandEnvelope,
  generation: number,
  workspaceRoot: string,
  configuredModel: string | undefined,
): CloudAgentCommandEnvelope {
  if (command.generation !== generation) {
    throw new Error(
      `Cloud Agent command generation ${command.generation} does not match Host generation ${generation}.`,
    );
  }
  if (command.commandType !== "StartSession" && command.commandType !== "ResumeSession") {
    return command;
  }
  const rawRunnerInput = command.payload.runnerInput;
  if (!isRecord(rawRunnerInput)) throw new Error("Cloud Agent runnerInput is required.");
  const runnerInput = rawRunnerInput as unknown as RunnerInput;
  if (resolve(runnerInput.workspaceDirectory) !== workspaceRoot) {
    throw new Error("Cloud Agent runnerInput Workspace does not match Host authority.");
  }
  const commandModel = runnerInput.workload.model?.trim();
  if (configuredModel && commandModel && commandModel !== configuredModel) {
    throw new Error("Cloud Agent runnerInput model conflicts with Host configuration.");
  }
  if (!configuredModel || commandModel === configuredModel) return command;
  return {
    ...command,
    payload: {
      ...command.payload,
      runnerInput: {
        ...runnerInput,
        workload: { ...runnerInput.workload, model: configuredModel },
      },
    },
  };
}

function assertProvider(expected: PortableProviderKind, actual: unknown): void {
  const normalized = typeof actual === "string" ? actual.trim().toLowerCase() : "";
  const matches =
    expected === "codex"
      ? normalized === "codex"
      : normalized === "claude" || normalized === "claudeagent";
  if (!matches)
    throw new Error(`Provider plugin ${expected} cannot execute provider ${String(actual)}.`);
}

function readArtifactCandidate(
  payload: Readonly<Record<string, unknown>>,
): Parameters<NonNullable<CloudAgentHostServices["acceptArtifact"]>>[0] | undefined {
  const artifact = payload.artifact;
  if (!isRecord(artifact) || typeof artifact.path !== "string") return undefined;
  const relativePath = portableRelativePath(artifact.path);
  if (!relativePath) return undefined;
  if (artifact.sourceRoot !== "workspace" && artifact.sourceRoot !== "runtime-output") {
    return undefined;
  }
  const sourceRoot = artifact.sourceRoot;
  const kind =
    artifact.kind === "diff" ||
    artifact.kind === "terminal-log" ||
    artifact.kind === "terminal_log" ||
    artifact.kind === "provider-output" ||
    artifact.kind === "provider_output"
      ? artifact.kind
      : artifact.kind === "generated-file" || artifact.kind === "generated_file"
        ? "generated-file"
        : undefined;
  if (!kind) return undefined;
  const normalizedKind = kind.replaceAll("_", "-") as
    | "diff"
    | "generated-file"
    | "terminal-log"
    | "provider-output";
  const reportedSize =
    typeof artifact.reportedSize === "number" ? artifact.reportedSize : undefined;
  if (reportedSize !== undefined && (!Number.isSafeInteger(reportedSize) || reportedSize < 0)) {
    return undefined;
  }
  const sha256 = typeof artifact.sha256 === "string" ? artifact.sha256 : undefined;
  if (artifact.sha256 !== undefined && (!sha256 || !/^[0-9a-f]{64}$/iu.test(sha256))) {
    return undefined;
  }
  return {
    sourceRoot,
    relativePath,
    kind: normalizedKind,
    contentType:
      typeof artifact.contentType === "string" ? artifact.contentType : "application/octet-stream",
    ...(reportedSize !== undefined ? { reportedSize } : {}),
    ...(sha256 ? { sha256: sha256.toLowerCase() } : {}),
  };
}

function portableRelativePath(value: string): string | undefined {
  const normalized = value.replaceAll("\\", "/").trim();
  if (!normalized || normalized.startsWith("/") || /^[a-z]:\//iu.test(normalized)) return undefined;
  const segments = normalized.split("/");
  return segments.some((segment) => !segment || segment === "." || segment === "..")
    ? undefined
    : normalized;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

class AsyncMessageQueue<T> implements AsyncIterable<T>, AsyncIterator<T> {
  static readonly maximumBufferedValues = 2_048;
  readonly #values: T[] = [];
  readonly #waiters: Array<(result: IteratorResult<T>) => void> = [];
  readonly #isPriority: (value: T) => boolean;
  readonly #onOverflow: () => void;
  #closed = false;

  constructor(isPriority: (value: T) => boolean, onOverflow: () => void) {
    this.#isPriority = isPriority;
    this.#onOverflow = onOverflow;
  }

  push(value: T): void {
    if (this.#closed) return;
    const waiter = this.#waiters.shift();
    if (waiter) waiter({ value, done: false });
    else {
      if (this.#values.length >= AsyncMessageQueue.maximumBufferedValues) {
        this.#onOverflow();
        // Terminal receipts carry completion authority, so progress is evicted before receipts.
        const expendable = this.#values.findIndex((buffered) => !this.#isPriority(buffered));
        if (expendable < 0 && !this.#isPriority(value)) return;
        this.#values.splice(expendable < 0 ? 0 : expendable, 1);
      }
      this.#values.push(value);
    }
  }

  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    for (const waiter of this.#waiters.splice(0)) waiter({ value: undefined, done: true });
  }

  next(): Promise<IteratorResult<T>> {
    const value = this.#values.shift();
    if (value !== undefined) return Promise.resolve({ value, done: false });
    if (this.#closed) return Promise.resolve({ value: undefined, done: true });
    return new Promise((resolve) => this.#waiters.push(resolve));
  }

  [Symbol.asyncIterator](): AsyncIterator<T> {
    return this;
  }
}

function isTerminalMessage(message: CloudAgentMessageEnvelope): boolean {
  return message.messageType === "Result" || message.messageType === "Error";
}

function terminalMessage(
  messages: ReadonlyArray<CloudAgentMessageEnvelope>,
): CloudAgentMessageEnvelope {
  const terminal = messages.at(-1);
  if (!terminal || !isTerminalMessage(terminal)) {
    throw new Error("Cloud Agent Provider did not emit a terminal message.");
  }
  return terminal;
}

function trackTask<T>(tasks: Set<Promise<unknown>>, task: Promise<T>): Promise<T> {
  tasks.add(task);
  task.then(
    () => tasks.delete(task),
    () => tasks.delete(task),
  );
  return task;
}

async function settleTasks(tasks: Set<Promise<unknown>>): Promise<void> {
  while (tasks.size > 0) await Promise.allSettled(tasks);
}

function commandAbortError(): Error {
  const error = new Error("Cloud Agent command was aborted.");
  error.name = "AbortError";
  return error;
}

function raceCommandWithAbort<T>(
  operation: Promise<T>,
  signal: AbortSignal,
  onAbort: () => void,
): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    let settled = false;
    const finish = (action: () => void) => {
      if (settled) return;
      settled = true;
      signal.removeEventListener("abort", abort);
      action();
    };
    const abort = () =>
      finish(() => {
        onAbort();
        reject(commandAbortError());
      });
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) abort();
    operation.then(
      (value) => finish(() => resolve(value)),
      (cause) => finish(() => reject(cause)),
    );
  });
}

function interruptCommand(
  command: CloudAgentCommandEnvelope,
): CloudAgentCommandEnvelope | undefined {
  if (
    command.commandType !== "SendTurn" &&
    command.commandType !== "CompactSession" &&
    command.commandType !== "StartReview"
  ) {
    return undefined;
  }
  return {
    ...command,
    requestId: `${command.requestId}:abort`,
    commandId: `${command.commandId}:abort`,
    commandType: "InterruptTurn",
    occurredAt: new Date().toISOString(),
    payload: { targetCommandId: command.commandId },
  };
}
