import type { Readable, Writable } from "node:stream";

import {
  CLOUD_AGENT_COMMAND_TYPES,
  CLOUD_AGENT_MAX_COMMAND_BYTES,
  CLOUD_AGENT_MAX_MESSAGE_BYTES,
  CLOUD_AGENT_PROTOCOL_VERSION,
  type CloudAgentCommandEnvelope,
  type CloudAgentErrorCode,
  type CloudAgentMessageEnvelope,
} from "@synara/cloud-agent-protocol";
import type {
  CloudAgentHostServices,
  CloudAgentProviderSession,
} from "@synara/cloud-agent-provider-api";

import { createBoundedNdjsonWriter } from "./ndjsonWriter";
import type { CloudAgentRuntimeV1 } from "./runtime";

export interface CloudAgentRuntimeStdioOptions {
  readonly source?: Readable;
  readonly output?: Writable;
  readonly diagnostics?: Writable;
  readonly allowedProviders?: ReadonlyArray<string>;
  readonly environment?: Readonly<Record<string, string | undefined>>;
}

type ActiveSession = {
  readonly providerKind: string;
  readonly generation: number;
  readonly session: CloudAgentProviderSession;
  readonly eventPump: Promise<void>;
};

const FATAL_UTF8_DECODER = new TextDecoder("utf-8", { fatal: true });

/** Runs the public Plugin ABI registry over bounded Protocol v2 NDJSON. */
export async function runCloudAgentRuntimeStdio(
  runtime: CloudAgentRuntimeV1,
  options: CloudAgentRuntimeStdioOptions = {},
): Promise<void> {
  const source = options.source ?? process.stdin;
  const output = options.output ?? process.stdout;
  const diagnostics = options.diagnostics ?? process.stderr;
  const allowed = allowedProviderSet(runtime, options.allowedProviders);
  const writer = createBoundedNdjsonWriter({
    target: output,
    maximumMessageBytes: CLOUD_AGENT_MAX_MESSAGE_BYTES,
  });
  const sessions = new Map<string, ActiveSession>();
  const commandTasks = new Set<Promise<void>>();
  const lifecycleBarriers = new Map<string, Promise<void>>();
  const credentialFd = readCredentialFd(options.environment ?? process.env);
  const shutdown = new AbortController();
  let credentialConsumed = false;
  let fatalLatched = false;
  let fatalCause: unknown;
  let readingSource = true;
  const latchFatal = (cause: unknown) => {
    if (fatalLatched) return;
    fatalLatched = true;
    fatalCause = cause;
    shutdown.abort(cause);
    if (readingSource && !source.destroyed) source.destroy(asError(cause));
  };
  const onOutputError = (cause: Error) => latchFatal(cause);
  const onOutputClose = () =>
    latchFatal(new Error("Provider Host stdout closed before Runtime shutdown."));
  output.once("error", onOutputError);
  output.once("close", onOutputClose);
  try {
    for await (const line of boundedNdjsonLines(source, CLOUD_AGENT_MAX_COMMAND_BYTES)) {
      if (fatalLatched) break;
      if (!line.trim()) continue;
      let command: CloudAgentCommandEnvelope;
      try {
        command = decodeCommand(line);
      } catch (cause) {
        diagnostics.write(`cloud-agent-runtime: ${errorText(cause)}\n`);
        throw cause;
      }
      schedule(command);
    }
  } catch (cause) {
    latchFatal(cause);
  } finally {
    readingSource = false;
    shutdown.abort(new Error(fatalLatched ? "Runtime failed." : "Runtime stdin closed."));
    const activeSessions = [...sessions.entries()];
    for (const [executionId, active] of activeSessions) {
      if (sessions.get(executionId) === active) sessions.delete(executionId);
    }
    // Close first: an active execute may only settle after its Provider is
    // interrupted. Waiting for command tasks before close would deadlock the
    // fatal-input and incomplete-EOF paths.
    await Promise.allSettled(
      activeSessions.map(([, { session }]) => session.close("stdio closed")),
    );
    await Promise.allSettled(commandTasks);
    await drainRegisteredSessions(sessions);
    try {
      await writer.flush();
    } catch (cause) {
      latchFatal(cause);
    } finally {
      output.off("error", onOutputError);
      output.off("close", onOutputClose);
    }
  }
  if (fatalLatched) throw fatalCause;

  function schedule(command: CloudAgentCommandEnvelope): void {
    const lifecycle =
      command.commandType === "StartSession" ||
      command.commandType === "ResumeSession" ||
      command.commandType === "StopSession";
    const prior = lifecycleBarriers.get(command.executionId) ?? Promise.resolve();
    const task = (lifecycle || command.commandType !== "Describe" ? prior : Promise.resolve()).then(
      () => (fatalLatched ? undefined : dispatch(command)),
    );
    if (lifecycle) {
      const barrier = task
        .catch(() => undefined)
        .finally(() => {
          if (lifecycleBarriers.get(command.executionId) === barrier) {
            lifecycleBarriers.delete(command.executionId);
          }
        });
      lifecycleBarriers.set(command.executionId, barrier);
    }
    commandTasks.add(task);
    task.then(
      () => commandTasks.delete(task),
      (cause) => {
        commandTasks.delete(task);
        latchFatal(cause);
      },
    );
  }

  async function dispatch(command: CloudAgentCommandEnvelope): Promise<void> {
    try {
      if (command.commandType === "Describe") {
        const providerKind = requiredProvider(command.payload.provider);
        assertAllowedProvider(allowed, providerKind);
        const descriptor = await runtime.describe(providerKind);
        if (fatalLatched) return;
        writer.enqueue(resultMessage(command, { descriptor }));
        return;
      }

      let active = sessions.get(command.executionId);
      if (command.commandType === "StartSession" || command.commandType === "ResumeSession") {
        const binding = runnerBinding(command);
        assertAllowedProvider(allowed, binding.providerKind);
        if (active) {
          await active.session.close("replaced");
          await active.eventPump;
          sessions.delete(command.executionId);
        }
        const creation = runtime.createSession(
          binding.providerKind,
          {
            hostInstanceId: command.executionId,
            hostThreadId: binding.hostThreadId,
            configuration: binding.configuration,
          },
          hostServices(binding, command.generation, diagnostics, {
            async acquire(request) {
              if (
                request.providerKind !== binding.providerKind ||
                request.operation !== "session" ||
                request.generation !== command.generation
              ) {
                throw runtimeFailure(
                  "credential_invalid",
                  "Credential request does not match the active Provider session binding.",
                );
              }
              if (credentialFd === undefined) return null;
              if (credentialConsumed) {
                throw runtimeFailure(
                  "credential_invalid",
                  "Runtime credential descriptor has already been consumed.",
                );
              }
              credentialConsumed = true;
              return {
                delivery: "anonymous-fd",
                fd: credentialFd,
                async [Symbol.asyncDispose]() {},
              };
            },
          }),
          shutdown.signal,
        );
        const session = await sessionCreationBeforeShutdown(creation, shutdown.signal);
        const eventPump = pumpSessionEvents(session, writer.enqueue);
        eventPump.then(undefined, latchFatal);
        active = {
          providerKind: binding.providerKind,
          generation: command.generation,
          session,
          eventPump,
        };
        sessions.set(command.executionId, active);
      }
      if (!active) throw runtimeFailure("session_resume_invalid", "No active Provider Session.");
      if (active.generation !== command.generation) {
        throw runtimeFailure("protocol_violation", "Command generation does not match Session.");
      }
      const terminal = await active.session.execute(command);
      // Plugin sessions publish every message, including the terminal, to events.
      // Waiting for execute here is the command receipt barrier; the event pump owns output.
      if (terminal.messageType !== "Result" && terminal.messageType !== "Error") {
        throw runtimeFailure("protocol_violation", "Provider returned a non-terminal receipt.");
      }
      if (command.commandType === "StopSession") {
        await active.session.close("stopped");
        await active.eventPump;
        sessions.delete(command.executionId);
      }
    } catch (cause) {
      writer.enqueue(errorMessage(command, cause));
    }
  }
}

async function* boundedNdjsonLines(
  source: Readable,
  maximumLineBytes: number,
): AsyncGenerator<string> {
  let buffer = Buffer.alloc(0);
  for await (const value of source) {
    const chunk = Buffer.isBuffer(value) ? value : Buffer.from(value);
    buffer = Buffer.concat([buffer, chunk]);
    while (true) {
      const newline = buffer.indexOf(0x0a);
      if (newline < 0) break;
      if (newline > maximumLineBytes) throw new Error("Command exceeds the negotiated size limit.");
      const frame = buffer.subarray(0, newline);
      buffer = buffer.subarray(newline + 1);
      yield decodeUtf8Frame(frame).replace(/\r$/u, "");
    }
    if (buffer.length > maximumLineBytes) {
      throw new Error("Command exceeds the negotiated size limit.");
    }
  }
  if (buffer.length > 0) throw new Error("Runtime stdin ended with an incomplete NDJSON frame.");
}

function decodeUtf8Frame(frame: Uint8Array): string {
  try {
    return FATAL_UTF8_DECODER.decode(frame);
  } catch {
    throw new Error("Command frame is not valid UTF-8.");
  }
}

function allowedProviderSet(
  runtime: CloudAgentRuntimeV1,
  configured: ReadonlyArray<string> | undefined,
): ReadonlySet<string> {
  const registered = new Set(runtime.providerKinds);
  const allowed = new Set(configured ?? runtime.providerKinds);
  for (const providerKind of allowed) {
    if (!registered.has(providerKind)) {
      throw new Error(`Allowed Provider ${providerKind} is not registered.`);
    }
  }
  return allowed;
}

function assertAllowedProvider(allowed: ReadonlySet<string>, providerKind: string): void {
  if (!allowed.has(providerKind)) {
    throw runtimeFailure("provider_not_installed", `Provider ${providerKind} is disabled.`);
  }
}

function decodeCommand(line: string): CloudAgentCommandEnvelope {
  if (Buffer.byteLength(line, "utf8") > CLOUD_AGENT_MAX_COMMAND_BYTES) {
    throw new Error("Command exceeds the negotiated size limit.");
  }
  const value: unknown = JSON.parse(line);
  if (!isRecord(value)) throw new Error("Command must be an object.");
  const protocol = isRecord(value.protocolVersion) ? value.protocolVersion : undefined;
  if (
    protocol?.major !== CLOUD_AGENT_PROTOCOL_VERSION.major ||
    typeof protocol.minor !== "number" ||
    protocol.minor > CLOUD_AGENT_PROTOCOL_VERSION.minor ||
    typeof value.requestId !== "string" ||
    typeof value.executionId !== "string" ||
    !Number.isSafeInteger(value.generation) ||
    typeof value.commandId !== "string" ||
    typeof value.occurredAt !== "string" ||
    !CLOUD_AGENT_COMMAND_TYPES.includes(value.commandType as never) ||
    !isRecord(value.payload)
  ) {
    throw new Error("Command does not match Protocol v2.");
  }
  return value as unknown as CloudAgentCommandEnvelope;
}

function runnerBinding(command: CloudAgentCommandEnvelope): {
  providerKind: string;
  hostThreadId: string;
  workspaceRoot: string;
  runtimeOutputRoot?: string;
  providerStateRoot?: string;
  configuration: Readonly<Record<string, unknown>>;
} {
  const input = isRecord(command.payload.runnerInput) ? command.payload.runnerInput : undefined;
  const workload = isRecord(input?.workload) ? input.workload : undefined;
  const execution = isRecord(input?.execution) ? input.execution : undefined;
  const providerKind = requiredProvider(workload?.provider);
  if (typeof input?.workspaceDirectory !== "string" || !input.workspaceDirectory.trim()) {
    throw runtimeFailure("workspace_invalid", "runnerInput Workspace is required.");
  }
  const model =
    typeof workload?.model === "string" && workload.model.trim()
      ? workload.model.trim()
      : undefined;
  return {
    providerKind,
    hostThreadId:
      typeof execution?.id === "string" && execution.id.trim() ? execution.id : command.executionId,
    workspaceRoot: input.workspaceDirectory,
    ...(typeof input.runtimeOutputDirectory === "string"
      ? { runtimeOutputRoot: input.runtimeOutputDirectory }
      : {}),
    ...(typeof input.providerStateDirectory === "string"
      ? { providerStateRoot: input.providerStateDirectory }
      : {}),
    configuration: model ? { model } : {},
  };
}

function hostServices(
  binding: ReturnType<typeof runnerBinding>,
  generation: number,
  diagnostics: Writable,
  credential: CloudAgentHostServices["credential"],
): CloudAgentHostServices {
  return {
    workspace: {
      authority: "host",
      root: binding.workspaceRoot,
      ...(binding.runtimeOutputRoot ? { runtimeOutputRoot: binding.runtimeOutputRoot } : {}),
      ...(binding.providerStateRoot ? { providerStateRoot: binding.providerStateRoot } : {}),
      generation,
      readOnly: false,
    },
    credential,
    log: {
      debug: (message) => diagnostics.write(`cloud-agent-runtime: debug: ${message}\n`),
      info: (message) => diagnostics.write(`cloud-agent-runtime: info: ${message}\n`),
      warn: (message) => diagnostics.write(`cloud-agent-runtime: warn: ${message}\n`),
      error: (message) => diagnostics.write(`cloud-agent-runtime: error: ${message}\n`),
    },
  };
}

async function drainRegisteredSessions(sessions: Map<string, ActiveSession>): Promise<void> {
  while (sessions.size > 0) {
    const snapshot = [...sessions.entries()];
    for (const [executionId, active] of snapshot) {
      if (sessions.get(executionId) === active) sessions.delete(executionId);
    }
    await Promise.allSettled(snapshot.map(([, { session }]) => session.close("stdio closed")));
    await Promise.allSettled(snapshot.map(([, { eventPump }]) => eventPump));
  }
}

function sessionCreationBeforeShutdown(
  creation: Promise<CloudAgentProviderSession>,
  signal: AbortSignal,
): Promise<CloudAgentProviderSession> {
  return new Promise((resolve, reject) => {
    let settled = false;
    const onAbort = () => {
      if (settled) return;
      settled = true;
      signal.removeEventListener("abort", onAbort);
      void creation.then(closeLateSession, () => undefined);
      reject(runtimeFailure("cancelled", "Runtime closed while creating the Provider Session."));
    };
    signal.addEventListener("abort", onAbort, { once: true });
    if (signal.aborted) {
      onAbort();
      return;
    }
    creation.then(
      (session) => {
        if (settled) {
          closeLateSession(session);
          return;
        }
        if (signal.aborted) {
          settled = true;
          signal.removeEventListener("abort", onAbort);
          closeLateSession(session);
          reject(
            runtimeFailure("cancelled", "Runtime closed while creating the Provider Session."),
          );
          return;
        }
        settled = true;
        signal.removeEventListener("abort", onAbort);
        resolve(session);
      },
      (cause) => {
        if (settled) return;
        settled = true;
        signal.removeEventListener("abort", onAbort);
        reject(cause);
      },
    );
  });
}

function closeLateSession(session: CloudAgentProviderSession): void {
  void session.close("stdio closing").catch(() => undefined);
}

async function pumpSessionEvents(
  session: CloudAgentProviderSession,
  emit: (message: CloudAgentMessageEnvelope) => void,
): Promise<void> {
  for await (const message of session.events) emit(message);
}

function readCredentialFd(
  environment: Readonly<Record<string, string | undefined>>,
): number | undefined {
  const value = environment.SYNARA_PROVIDER_CREDENTIAL_FD?.trim();
  if (!value) return undefined;
  const fd = Number(value);
  if (!Number.isSafeInteger(fd) || fd < 3 || fd > 1_024) {
    throw new Error("SYNARA_PROVIDER_CREDENTIAL_FD is invalid.");
  }
  return fd;
}

function requiredProvider(value: unknown): string {
  if (typeof value !== "string" || !value.trim()) {
    throw runtimeFailure("provider_not_installed", "Provider is required.");
  }
  return value.trim();
}

function resultMessage(
  command: CloudAgentCommandEnvelope,
  payload: Readonly<Record<string, unknown>>,
): CloudAgentMessageEnvelope {
  return {
    ...messageBase(command),
    messageType: "Result",
    payload,
  };
}

function errorMessage(
  command: CloudAgentCommandEnvelope,
  cause: unknown,
): CloudAgentMessageEnvelope {
  const failure =
    cause instanceof RuntimeStdioFailure
      ? cause
      : runtimeFailure("internal_error", errorText(cause));
  return {
    ...messageBase(command),
    messageType: "Error",
    error: {
      code: failure.code,
      message: failure.message,
      retryable: failure.code === "provider_unavailable",
      requiresNewExecution: failure.code !== "capability_unsupported",
      requiresUserAction: failure.code === "provider_not_installed",
      canReconstructFromHistory: true,
      canMoveWorker: true,
    },
  };
}

function messageBase(command: CloudAgentCommandEnvelope) {
  return {
    requestId: command.requestId,
    protocolVersion: CLOUD_AGENT_PROTOCOL_VERSION,
    executionId: command.executionId,
    generation: command.generation,
    commandId: command.commandId,
    occurredAt: new Date().toISOString(),
  };
}

class RuntimeStdioFailure extends Error {
  constructor(
    readonly code: CloudAgentErrorCode,
    message: string,
  ) {
    super(message);
  }
}

function runtimeFailure(code: CloudAgentErrorCode, message: string): RuntimeStdioFailure {
  return new RuntimeStdioFailure(code, message);
}

function errorText(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause);
}

function asError(cause: unknown): Error {
  return cause instanceof Error ? cause : new Error(String(cause));
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
