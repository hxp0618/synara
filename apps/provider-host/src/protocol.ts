// FILE: protocol.ts
// Purpose: Implements Provider Host Protocol v2 negotiation and command envelopes.

import { spawnSync } from "node:child_process";
import { createInterface } from "node:readline";
import type { Readable } from "node:stream";

import {
  PROVIDER_CAPABILITY_IDS,
  PROVIDER_HOST_MAX_COMMAND_BYTES,
  PROVIDER_HOST_MAX_MESSAGE_BYTES,
  PROVIDER_HOST_PROTOCOL_VERSION,
  ProviderHostCommandEnvelope,
  type ProviderCapabilityMap,
  type ProviderCapabilitySupport,
  type ProviderHostCommandEnvelope as ProviderHostCommand,
  type ProviderHostDescriptor,
  type ProviderHostError,
  type ProviderHostMessageEnvelope,
} from "@synara/contracts/provider-host";
import type { ProviderKind } from "@synara/contracts";
import { Schema } from "effect";

import {
  runProviderHost,
  validateRunnerInput,
  type RunnerCredential,
  type RunnerInput,
  type RunnerMessage,
} from "./providerHost";

const decodeCommand = Schema.decodeUnknownSync(ProviderHostCommandEnvelope);
const HOST_BUILD_VERSION = process.env.SYNARA_PROVIDER_HOST_BUILD_VERSION?.trim() || "0.2.0-dev";

type ProtocolState = {
  sessionInput: RunnerInput | null;
  terminalByCommandId: Map<string, ProviderHostMessageEnvelope>;
};

type ProtocolHandler = (
  command: ProviderHostCommand,
) => Promise<ReadonlyArray<ProviderHostMessageEnvelope>>;

export function providerHostDescriptor(provider: ProviderKind): ProviderHostDescriptor {
  const remote = provider === "codex" || provider === "claudeAgent";
  return {
    protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
    hostBuildVersion: HOST_BUILD_VERSION,
    capabilityDescriptor: {
      provider,
      supportTier: remote ? "experimental" : "local-only",
      adapterVersion: remote ? `${provider}-remote-v2` : `${provider}-local-only`,
      ...(remote ? { providerCliVersion: readProviderCliVersion(provider) } : {}),
      capabilities: capabilityMapForProvider(provider),
    },
    maximumCommandBytes: PROVIDER_HOST_MAX_COMMAND_BYTES,
    maximumMessageBytes: PROVIDER_HOST_MAX_MESSAGE_BYTES,
    runtimeEventVersions: { minimum: 1, maximum: 1 },
    credentialDeliveryModes: remote ? ["anonymous-fd"] : [],
    resumeStrategies: remote
      ? ["native-cursor", "authoritative-history"]
      : [],
  };
}

export function capabilityMapForProvider(provider: ProviderKind): ProviderCapabilityMap {
  const capabilities = Object.fromEntries(
    PROVIDER_CAPABILITY_IDS.map((capability) => [capability, "unsupported"]),
  ) as Record<(typeof PROVIDER_CAPABILITY_IDS)[number], ProviderCapabilitySupport>;

  if (provider === "codex" || provider === "claudeAgent") {
    Object.assign(capabilities, {
      discovery: "native",
      "start-session": "native",
      "resume-session": "native",
      "send-turn": "native",
      "read-history": "emulated",
      "model-switch": "native",
      "tool-events": "emulated",
      "usage-events": "native",
      "credential-injection": "native",
      "authoritative-history-reconstruction": "emulated",
      "worker-migration": "emulated",
    } satisfies Partial<Record<(typeof PROVIDER_CAPABILITY_IDS)[number], ProviderCapabilitySupport>>);
  }

  return capabilities;
}

export function createProviderHostProtocolHandler(input: {
  credential: RunnerCredential | null;
  emit: (message: ProviderHostMessageEnvelope) => void;
}): ProtocolHandler {
  const state: ProtocolState = {
    sessionInput: null,
    terminalByCommandId: new Map(),
  };

  return async (command) => {
    const cached = state.terminalByCommandId.get(command.commandId);
    if (cached) {
      input.emit(cached);
      return [cached];
    }

    const emitted: ProviderHostMessageEnvelope[] = [];
    const emit = (message: ProviderHostMessageEnvelope) => {
      emitted.push(message);
      input.emit(message);
    };

    const terminal = await executeCommand(command, state, input.credential, emit).catch((error) =>
      errorMessage(command, classifyProviderHostError(error)),
    );
    state.terminalByCommandId.set(command.commandId, terminal);
    emitted.push(terminal);
    input.emit(terminal);
    return emitted;
  };
}

export async function runProviderHostProtocolV2(input: {
  source: Readable;
  credential: RunnerCredential | null;
  emit: (message: ProviderHostMessageEnvelope) => void;
}): Promise<void> {
  const handle = createProviderHostProtocolHandler({ credential: input.credential, emit: input.emit });
  const lines = createInterface({ input: input.source, crlfDelay: Infinity });

  for await (const line of lines) {
    if (!line.trim()) continue;
    if (Buffer.byteLength(line) > PROVIDER_HOST_MAX_COMMAND_BYTES) {
      input.emit(
        errorMessage(protocolFallbackCommand(), {
          code: "protocol_violation",
          message: "Provider Host command exceeds the negotiated size limit.",
          retryable: false,
          requiresNewExecution: true,
          requiresUserAction: false,
          canReconstructFromHistory: true,
          canMoveWorker: true,
        }),
      );
      continue;
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      input.emit(
        errorMessage(protocolFallbackCommand(), {
          code: "protocol_violation",
          message: "Provider Host command is not valid JSON.",
          retryable: false,
          requiresNewExecution: true,
          requiresUserAction: false,
          canReconstructFromHistory: true,
          canMoveWorker: true,
        }),
      );
      continue;
    }

    let command: ProviderHostCommand;
    try {
      command = decodeCommand(parsed);
    } catch {
      input.emit(
        errorMessage(protocolFallbackCommand(parsed), {
          code: "protocol_violation",
          message: "Provider Host command does not match the v2 envelope.",
          retryable: false,
          requiresNewExecution: true,
          requiresUserAction: false,
          canReconstructFromHistory: true,
          canMoveWorker: true,
        }),
      );
      continue;
    }
    await handle(command);
  }
}

async function executeCommand(
  command: ProviderHostCommand,
  state: ProtocolState,
  credential: RunnerCredential | null,
  emit: (message: ProviderHostMessageEnvelope) => void,
): Promise<ProviderHostMessageEnvelope> {
  assertCompatibleProtocol(command);

  switch (command.commandType) {
    case "Describe": {
      const provider = readProvider(command.payload.provider);
      return resultMessage(command, { descriptor: providerHostDescriptor(provider) });
    }
    case "StartSession":
    case "ResumeSession": {
      const runnerInput = readRunnerInput(command.payload.runnerInput);
      const provider = readProvider(runnerInput.workload.provider);
      const descriptor = providerHostDescriptor(provider);
      if (descriptor.capabilityDescriptor.supportTier === "local-only") {
        throw new ProtocolFailure({
          code: "capability_unsupported",
          message: `${provider} is Local-only and cannot run in a remote Provider Host.`,
          retryable: false,
          requiresNewExecution: false,
          requiresUserAction: true,
          canReconstructFromHistory: false,
          canMoveWorker: false,
        });
      }
      if (
        command.commandType === "ResumeSession" &&
        !runnerInput.providerResumeCursor?.trim() &&
        (runnerInput.workload.conversationHistory?.length ?? 0) === 0
      ) {
        throw new ProtocolFailure({
          code: "session_resume_invalid",
          message: "ResumeSession requires a native Cursor or authoritative history.",
          retryable: false,
          requiresNewExecution: false,
          requiresUserAction: false,
          canReconstructFromHistory: false,
          canMoveWorker: true,
        });
      }
      state.sessionInput = {
        ...runnerInput,
        workload: { ...runnerInput.workload, inputText: "" },
      };
      return resultMessage(command, {
        provider,
        resumed: command.commandType === "ResumeSession",
      });
    }
    case "SendTurn": {
      if (!state.sessionInput) {
        throw new ProtocolFailure({
          code: "session_resume_invalid",
          message: "StartSession or ResumeSession must succeed before SendTurn.",
          retryable: false,
          requiresNewExecution: false,
          requiresUserAction: false,
          canReconstructFromHistory: true,
          canMoveWorker: true,
        });
      }
      const inputText = requiredString(command.payload.inputText, "SendTurn inputText");
      const runInput: RunnerInput = {
        ...state.sessionInput,
        workload: { ...state.sessionInput.workload, inputText },
      };
      let terminalResult: Extract<RunnerMessage, { type: "result" }> | undefined;
      await runProviderHost(runInput, credential, (message) => {
        if (message.type === "event") {
          emit(
            payloadMessage(command, "Event", {
              eventType: message.eventType,
              payload: message.payload,
            }),
          );
        } else {
          terminalResult = message;
        }
      });
      if (!terminalResult) {
        throw new Error("Provider Host completed without a result message");
      }
      const outputText = terminalResult.output.text;
      const history = [...(state.sessionInput.workload.conversationHistory ?? [])];
      history.push({ role: "user", text: inputText });
      if (typeof outputText === "string" && outputText.trim()) {
        history.push({ role: "assistant", text: outputText });
      }
      state.sessionInput = {
        ...state.sessionInput,
        ...(terminalResult.providerResumeCursor
          ? { providerResumeCursor: terminalResult.providerResumeCursor }
          : {}),
        workload: {
          ...state.sessionInput.workload,
          inputText: "",
          conversationHistory: history,
        },
      };
      return resultMessage(command, {
        output: terminalResult.output,
        ...(terminalResult.providerResumeCursor
          ? { providerResumeCursor: terminalResult.providerResumeCursor }
          : {}),
      });
    }
    case "StopSession":
      state.sessionInput = null;
      return resultMessage(command, { stopped: true });
    default:
      throw new ProtocolFailure({
        code: "capability_unsupported",
        message: `${command.commandType} is not implemented by this Provider Host adapter.`,
        retryable: false,
        requiresNewExecution: false,
        requiresUserAction: true,
        canReconstructFromHistory: true,
        canMoveWorker: true,
      });
  }
}

function assertCompatibleProtocol(command: ProviderHostCommand): void {
  if (command.protocolVersion.major !== PROVIDER_HOST_PROTOCOL_VERSION.major) {
    throw new ProtocolFailure({
      code: "provider_version_incompatible",
      message: `Provider Host Protocol major ${command.protocolVersion.major} is not supported.`,
      retryable: false,
      requiresNewExecution: true,
      requiresUserAction: true,
      canReconstructFromHistory: true,
      canMoveWorker: true,
    });
  }
}

function readRunnerInput(value: unknown): RunnerInput {
  if (!isRecord(value)) throw new Error("runnerInput is required");
  const input = value as RunnerInput;
  validateRunnerInput(input);
  return input;
}

function readProvider(value: unknown): ProviderKind {
  if (typeof value !== "string") throw new Error("provider is required");
  const normalized = value.trim().toLowerCase();
  if (normalized === "claude") return "claudeAgent";
  if (
    normalized === "codex" ||
    normalized === "claudeagent" ||
    normalized === "cursor" ||
    normalized === "gemini" ||
    normalized === "grok" ||
    normalized === "kilo" ||
    normalized === "opencode" ||
    normalized === "pi"
  ) {
    return normalized === "claudeagent" ? "claudeAgent" : normalized;
  }
  throw new ProtocolFailure({
    code: "provider_not_installed",
    message: `Provider ${value.trim()} is not known to this Provider Host.`,
    retryable: false,
    requiresNewExecution: false,
    requiresUserAction: true,
    canReconstructFromHistory: false,
    canMoveWorker: false,
  });
}

function readProviderCliVersion(provider: "codex" | "claudeAgent"): string {
  const executable = provider === "codex" ? "codex" : "claude";
  const result = spawnSync(executable, ["--version"], {
    encoding: "utf8",
    timeout: 5_000,
    env: { PATH: process.env.PATH, HOME: process.env.HOME },
  });
  const value = `${result.stdout ?? ""}\n${result.stderr ?? ""}`.trim();
  return value || "unavailable";
}

function payloadMessage(
  command: ProviderHostCommand,
  messageType: "Event" | "InteractionRequest" | "ArtifactCandidate" | "Checkpoint" | "Progress",
  payload: Record<string, unknown>,
): ProviderHostMessageEnvelope {
  return {
    ...messageBase(command),
    messageType,
    payload,
  } as ProviderHostMessageEnvelope;
}

function resultMessage(
  command: ProviderHostCommand,
  payload: Record<string, unknown>,
): ProviderHostMessageEnvelope {
  return {
    ...messageBase(command),
    messageType: "Result",
    payload,
  };
}

function errorMessage(
  command: ProviderHostCommand,
  error: ProviderHostError,
): ProviderHostMessageEnvelope {
  return {
    ...messageBase(command),
    messageType: "Error",
    error,
  };
}

function messageBase(command: ProviderHostCommand) {
  return {
    requestId: command.requestId,
    protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
    executionId: command.executionId,
    generation: command.generation,
    commandId: command.commandId,
    occurredAt: new Date().toISOString(),
  };
}

function protocolFallbackCommand(value?: unknown): ProviderHostCommand {
  const candidate = isRecord(value) ? value : {};
  return {
    requestId: safeWireString(candidate.requestId, "protocol-request"),
    protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
    executionId: safeWireString(candidate.executionId, "protocol-execution"),
    generation:
      typeof candidate.generation === "number" && candidate.generation >= 1
        ? Math.floor(candidate.generation)
        : 1,
    commandType: "Describe",
    commandId: safeWireString(candidate.commandId, "protocol-command") as ProviderHostCommand["commandId"],
    occurredAt: new Date().toISOString(),
    payload: {},
  };
}

function safeWireString(value: unknown, fallback: string): string {
  return typeof value === "string" && value.trim() ? value.trim().slice(0, 200) : fallback;
}

function classifyProviderHostError(error: unknown): ProviderHostError {
  if (error instanceof ProtocolFailure) return error.detail;
  const message = error instanceof Error ? error.message : String(error);
  const normalized = message.toLowerCase();
  if (normalized.includes("invalid jsonl") || normalized.includes("result message")) {
    return errorDetail("protocol_violation", message, false, true, false, true, true);
  }
  if (normalized.includes("credential")) {
    return errorDetail("credential_invalid", message, false, false, true, false, false);
  }
  if (normalized.includes("enoent") || normalized.includes("not found")) {
    return errorDetail("provider_not_installed", message, false, false, true, true, true);
  }
  if (normalized.includes("rate limit") || normalized.includes("rate-limit")) {
    return errorDetail("provider_rate_limited", message, true, true, false, true, true);
  }
  if (normalized.includes("auth") || normalized.includes("login")) {
    return errorDetail("authentication_required", message, false, false, true, true, true);
  }
  return errorDetail("provider_unavailable", message, true, true, false, true, true);
}

function errorDetail(
  code: ProviderHostError["code"],
  message: string,
  retryable: boolean,
  requiresNewExecution: boolean,
  requiresUserAction: boolean,
  canReconstructFromHistory: boolean,
  canMoveWorker: boolean,
): ProviderHostError {
  return {
    code,
    message: message.trim().slice(0, 2_000) || "Provider Host failed.",
    retryable,
    requiresNewExecution,
    requiresUserAction,
    canReconstructFromHistory,
    canMoveWorker,
  };
}

class ProtocolFailure extends Error {
  constructor(readonly detail: ProviderHostError) {
    super(detail.message);
  }
}

function requiredString(value: unknown, label: string): string {
  if (typeof value !== "string" || !value.trim()) throw new Error(`${label} is required`);
  return value;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
