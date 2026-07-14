// FILE: protocol.ts
// Purpose: Implements Provider Host Protocol v2 negotiation and command envelopes.

import { spawnSync } from "node:child_process";
import { createInterface } from "node:readline";
import type { Readable } from "node:stream";

import {
  PROVIDER_CAPABILITY_CATALOG,
  PROVIDER_CAPABILITY_IDS,
  PROVIDER_HOST_MAX_COMMAND_BYTES,
  PROVIDER_HOST_MAX_MESSAGE_BYTES,
  PROVIDER_HOST_PROTOCOL_VERSION,
  ProviderHostCommandEnvelope,
  type ProviderCapabilityCatalogEntry,
  type ProviderCapabilityMap,
  type ProviderHostCommandEnvelope as ProviderHostCommand,
  type ProviderHostDescriptor,
  type ProviderHostError,
  type ProviderHostMessageEnvelope,
  type ProviderHostProviderKind,
  type ProviderRuntimeCompatibleRange,
  type ProviderRuntimeDescriptor,
} from "@synara/contracts/provider-host";
import { PROVIDER_RUNTIME_EVENT_VERSION } from "@synara/contracts";
import { Schema } from "effect";

import providerHostPackage from "../package.json";
import {
  hasAuthoritativeResumeData,
  providerProcessEnvironment,
  startProviderHostRun,
  validateRunnerInput,
  type ProviderRunController,
  type RunnerCredential,
  type RunnerInput,
  type RunnerMessage,
} from "./providerHost";
import { normalizeRuntimeEventV2 } from "./runtimeEventV2";

const decodeCommand = Schema.decodeUnknownSync(ProviderHostCommandEnvelope);
const HOST_BUILD_VERSION = providerHostPackage.version;
const CLAUDE_AGENT_SDK_VERSION = providerHostPackage.dependencies["@anthropic-ai/claude-agent-sdk"];

export type CodexVersionProbeResult = {
  readonly available: boolean;
  readonly output?: string;
};

export type ProviderHostDescriptorOptions = {
  readonly environment?: Readonly<Record<string, string | undefined>>;
  readonly codexVersionProbe?: () => CodexVersionProbeResult;
  readonly claudeSdkVersion?: string;
  readonly hostBuildVersion?: string;
};

type ProviderDescriptorFactory = (provider: ProviderHostProviderKind) => ProviderHostDescriptor;

type ProtocolState = {
  sessionInput: RunnerInput | null;
  activeTurn: { commandId: string; run: ProviderRunController } | null;
  inFlightByCommandId: Map<string, Promise<ProviderHostMessageEnvelope>>;
  terminalByCommandId: Map<string, ProviderHostMessageEnvelope>;
};

type ProtocolHandler = (
  command: ProviderHostCommand,
) => Promise<ReadonlyArray<ProviderHostMessageEnvelope>>;

export function providerHostDescriptor(
  provider: ProviderHostProviderKind,
  options: ProviderHostDescriptorOptions = {},
): ProviderHostDescriptor {
  const catalogEntry = catalogEntryForProvider(provider);
  const remote = catalogEntry.supportTier !== "local-only";
  const runtime = runtimeDescriptor(catalogEntry, options);
  return {
    protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
    hostBuildVersion: options.hostBuildVersion?.trim() || HOST_BUILD_VERSION,
    capabilityDescriptor: {
      provider,
      supportTier: catalogEntry.supportTier,
      adapterVersion: catalogEntry.adapterVersion,
      ...(provider === "codex" && runtime.version ? { providerCliVersion: runtime.version } : {}),
      runtime,
      releasePolicy: releasePolicy(catalogEntry, options.environment ?? process.env),
      capabilities: capabilityMapForProvider(provider),
    },
    maximumCommandBytes: PROVIDER_HOST_MAX_COMMAND_BYTES,
    maximumMessageBytes: PROVIDER_HOST_MAX_MESSAGE_BYTES,
    runtimeEventVersions: {
      minimum: PROVIDER_RUNTIME_EVENT_VERSION,
      maximum: PROVIDER_RUNTIME_EVENT_VERSION,
    },
    credentialDeliveryModes: remote ? ["anonymous-fd"] : [],
    resumeStrategies: remote ? ["native-cursor", "authoritative-history"] : [],
  };
}

export function capabilityMapForProvider(
  provider: ProviderHostProviderKind,
): ProviderCapabilityMap {
  const capabilities = catalogEntryForProvider(provider).capabilities;
  return Object.fromEntries(
    PROVIDER_CAPABILITY_IDS.map((capability) => [capability, capabilities[capability]]),
  ) as ProviderCapabilityMap;
}

function catalogEntryForProvider(
  provider: ProviderHostProviderKind,
): ProviderCapabilityCatalogEntry {
  const entry = PROVIDER_CAPABILITY_CATALOG.providers.find(
    (candidate) => candidate.provider === provider,
  );
  if (!entry) throw new Error(`Provider capability catalog is missing ${provider}.`);
  return entry;
}

function runtimeDescriptor(
  entry: ProviderCapabilityCatalogEntry,
  options: ProviderHostDescriptorOptions,
): ProviderRuntimeDescriptor {
  const policy = entry.runtimePolicy;
  const compatibleRange = { ...policy.compatibleRange };

  if (entry.provider === "codex") {
    const probe = options.codexVersionProbe?.() ?? probeCodexVersion();
    const version = extractStableSemver(probe.output ?? "");
    return {
      kind: policy.kind,
      name: policy.name,
      ...(version ? { version } : {}),
      available: probe.available,
      versionSource: policy.versionSource,
      compatibleRange,
      compatible:
        probe.available && version !== undefined && isCompatibleVersion(version, compatibleRange),
    };
  }

  if (entry.provider === "claudeAgent") {
    const declaredVersion = (options.claudeSdkVersion ?? CLAUDE_AGENT_SDK_VERSION).trim();
    const version = extractStableSemver(declaredVersion);
    const available = declaredVersion.length > 0;
    return {
      kind: policy.kind,
      name: policy.name,
      ...(version ? { version } : {}),
      available,
      versionSource: policy.versionSource,
      compatibleRange,
      compatible:
        available && version !== undefined && isCompatibleVersion(version, compatibleRange),
    };
  }

  const buildVersion = (options.hostBuildVersion ?? HOST_BUILD_VERSION).trim();
  const available = buildVersion.length > 0;
  return {
    kind: policy.kind,
    name: policy.name,
    ...(available ? { version: buildVersion } : {}),
    available,
    versionSource: policy.versionSource,
    compatibleRange,
    compatible: available && isCompatibleVersion(buildVersion, compatibleRange),
  };
}

function releasePolicy(
  entry: ProviderCapabilityCatalogEntry,
  environment: Readonly<Record<string, string | undefined>>,
): ProviderHostDescriptor["capabilityDescriptor"]["releasePolicy"] {
  const requiresExplicitEnablement = entry.supportTier === "experimental";
  if (entry.supportTier === "local-only") {
    return { requiresExplicitEnablement, enabled: true };
  }
  if (!requiresExplicitEnablement) {
    return { requiresExplicitEnablement, enabled: true };
  }
  return {
    requiresExplicitEnablement,
    enabled: experimentalProviderAllowlist(environment).has(entry.provider),
  };
}

function experimentalProviderAllowlist(
  environment: Readonly<Record<string, string | undefined>>,
): ReadonlySet<ProviderHostProviderKind> {
  const providers = new Set<ProviderHostProviderKind>();
  for (const token of (environment.SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS ?? "").split(",")) {
    const normalized = token.trim().toLowerCase();
    if (normalized === "codex") providers.add("codex");
    if (normalized === "claude" || normalized === "claudeagent") {
      providers.add("claudeAgent");
    }
  }
  return providers;
}

function probeCodexVersion(): CodexVersionProbeResult {
  const result = spawnSync("codex", ["--version"], {
    encoding: "utf8",
    timeout: 5_000,
    env: providerProcessEnvironment(process.env),
  });
  const output = `${result.stdout ?? ""}\n${result.stderr ?? ""}`.trim();
  return {
    available: result.error === undefined && result.status !== null,
    ...(output ? { output } : {}),
  };
}

function extractStableSemver(value: string): string | undefined {
  const match = /(?:^|[^0-9])(\d+\.\d+\.\d+)(?![0-9A-Za-z.+-])/.exec(value);
  return match?.[1];
}

function isCompatibleVersion(version: string, range: ProviderRuntimeCompatibleRange): boolean {
  const parsed = parseSemver(version);
  const minimum = parseSemver(range.minimumInclusive);
  if (!parsed || !minimum || compareSemver(parsed, minimum) < 0) return false;
  if (!range.maximumExclusive) return true;
  const maximum = parseSemver(range.maximumExclusive);
  return maximum !== undefined && compareSemver(parsed, maximum) < 0;
}

type Semver = readonly [major: number, minor: number, patch: number];

function parseSemver(value: string): Semver | undefined {
  const match = /^(\d+)\.(\d+)\.(\d+)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/.exec(value.trim());
  if (!match) return undefined;
  return [Number(match[1]), Number(match[2]), Number(match[3])];
}

function compareSemver(left: Semver, right: Semver): number {
  return left[0] - right[0] || left[1] - right[1] || left[2] - right[2];
}

export function createProviderHostProtocolHandler(input: {
  credential: RunnerCredential | null;
  emit: (message: ProviderHostMessageEnvelope) => void;
  startRun?: typeof startProviderHostRun;
  descriptorForProvider?: ProviderDescriptorFactory;
}): ProtocolHandler {
  const state: ProtocolState = {
    sessionInput: null,
    activeTurn: null,
    inFlightByCommandId: new Map(),
    terminalByCommandId: new Map(),
  };
  const startRun = input.startRun ?? startProviderHostRun;
  const descriptorForProvider = input.descriptorForProvider ?? providerHostDescriptor;

  return async (command) => {
    const cached = state.terminalByCommandId.get(command.commandId);
    if (cached) {
      input.emit(cached);
      return [cached];
    }
    const inFlight = state.inFlightByCommandId.get(command.commandId);
    if (inFlight) {
      const terminal = await inFlight;
      input.emit(terminal);
      return [terminal];
    }

    const emitted: ProviderHostMessageEnvelope[] = [];
    const emit = (message: ProviderHostMessageEnvelope) => {
      emitted.push(message);
      input.emit(message);
    };

    const terminalPromise = executeCommand(
      command,
      state,
      input.credential,
      emit,
      startRun,
      descriptorForProvider,
    ).catch((error) => errorMessage(command, classifyProviderHostError(error)));
    state.inFlightByCommandId.set(command.commandId, terminalPromise);
    const terminal = await terminalPromise;
    state.inFlightByCommandId.delete(command.commandId);
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
  startRun?: typeof startProviderHostRun;
  descriptorForProvider?: ProviderDescriptorFactory;
}): Promise<void> {
  const handle = createProviderHostProtocolHandler({
    credential: input.credential,
    emit: input.emit,
    ...(input.startRun ? { startRun: input.startRun } : {}),
    ...(input.descriptorForProvider ? { descriptorForProvider: input.descriptorForProvider } : {}),
  });
  const lines = createInterface({ input: input.source, crlfDelay: Infinity });
  const inFlight = new Set<Promise<ReadonlyArray<ProviderHostMessageEnvelope>>>();

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
    const task = handle(command);
    inFlight.add(task);
    task.then(
      () => inFlight.delete(task),
      () => inFlight.delete(task),
    );
  }
  await Promise.all(inFlight);
}

async function executeCommand(
  command: ProviderHostCommand,
  state: ProtocolState,
  credential: RunnerCredential | null,
  emit: (message: ProviderHostMessageEnvelope) => void,
  startRun: typeof startProviderHostRun,
  descriptorForProvider: ProviderDescriptorFactory,
): Promise<ProviderHostMessageEnvelope> {
  assertCompatibleProtocol(command);

  switch (command.commandType) {
    case "Describe": {
      const provider = readProvider(command.payload.provider);
      return resultMessage(command, { descriptor: descriptorForProvider(provider) });
    }
    case "StartSession":
    case "ResumeSession": {
      const runnerInput = bindRunnerInputGeneration(
        readRunnerInput(command.payload.runnerInput),
        command.generation,
      );
      const provider = readProvider(runnerInput.workload.provider);
      const descriptor = descriptorForProvider(provider);
      assertProviderExecutionAllowed(provider, descriptor);
      if (
        command.commandType === "ResumeSession" &&
        !runnerInput.providerResumeCursor?.trim() &&
        !hasAuthoritativeResumeData(runnerInput.workload)
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
      if (state.activeTurn) {
        throw new ProtocolFailure({
          code: "protocol_violation",
          message: "Only one SendTurn command may be active in a Provider Session.",
          retryable: false,
          requiresNewExecution: true,
          requiresUserAction: false,
          canReconstructFromHistory: true,
          canMoveWorker: true,
        });
      }
      const run = startRun(runInput, credential, (message) => {
        if (message.type === "event") {
          emit(payloadMessage(command, "Event", normalizeRuntimeEventV2(message)));
        } else if (message.type === "interaction") {
          emit(
            payloadMessage(command, "InteractionRequest", {
              ...message.payload,
              interactionType: message.interactionType,
            }),
          );
        }
      });
      state.activeTurn = { commandId: command.commandId, run };
      let terminalResult: Extract<RunnerMessage, { type: "result" }>;
      try {
        terminalResult = await run.result;
      } finally {
        if (state.activeTurn?.commandId === command.commandId) state.activeTurn = null;
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
    case "SteerTurn": {
      const activeTurn = requireActiveTurn(state, command.commandType);
      validateTargetCommandId(command.payload.targetCommandId, activeTurn.commandId);
      if (!activeTurn.run.steer) {
        throw unsupportedActiveTurnCommand(command.commandType);
      }
      const inputText = requiredString(command.payload.inputText, "SteerTurn inputText");
      await activeTurn.run.steer({ inputText });
      return resultMessage(command, {
        steered: true,
        targetCommandId: activeTurn.commandId,
      });
    }
    case "InterruptTurn": {
      const activeTurn = requireActiveTurn(state, command.commandType);
      validateTargetCommandId(command.payload.targetCommandId, activeTurn.commandId);
      activeTurn.run.interrupt();
      const providerResumeCursor = activeTurn.run.getResumeCursor?.();
      return resultMessage(command, {
        interrupted: true,
        targetCommandId: activeTurn.commandId,
        ...(providerResumeCursor ? { providerResumeCursor } : {}),
      });
    }
    case "ResolveApproval": {
      const activeTurn = requireActiveTurn(state, command.commandType);
      if (!activeTurn.run.resolveApproval) {
        throw unsupportedInteractiveCommand(command.commandType);
      }
      validateResolutionCommandPayload(command.payload, command.commandType);
      await activeTurn.run.resolveApproval(command.payload);
      return resultMessage(command, {
        acknowledged: true,
        requestId: command.payload.requestId,
      });
    }
    case "ResolveUserInput": {
      const activeTurn = requireActiveTurn(state, command.commandType);
      if (!activeTurn.run.resolveUserInput) {
        throw unsupportedInteractiveCommand(command.commandType);
      }
      validateResolutionCommandPayload(command.payload, command.commandType);
      await activeTurn.run.resolveUserInput(command.payload);
      return resultMessage(command, {
        acknowledged: true,
        requestId: command.payload.requestId,
      });
    }
    case "StopSession":
      state.activeTurn?.run.interrupt();
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

function assertProviderExecutionAllowed(
  provider: ProviderHostProviderKind,
  descriptor: ProviderHostDescriptor,
): void {
  const capabilityDescriptor = descriptor.capabilityDescriptor;
  if (capabilityDescriptor.supportTier === "local-only") {
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
    capabilityDescriptor.releasePolicy.requiresExplicitEnablement &&
    !capabilityDescriptor.releasePolicy.enabled
  ) {
    throw new ProtocolFailure({
      code: "capability_unsupported",
      message: `${provider} remote execution is experimental and is not explicitly enabled on this Provider Host.`,
      retryable: false,
      requiresNewExecution: false,
      requiresUserAction: true,
      canReconstructFromHistory: true,
      canMoveWorker: true,
    });
  }
  if (!capabilityDescriptor.runtime.available) {
    throw new ProtocolFailure({
      code: "provider_not_installed",
      message: `${capabilityDescriptor.runtime.name} is not available on this Provider Host.`,
      retryable: false,
      requiresNewExecution: false,
      requiresUserAction: true,
      canReconstructFromHistory: true,
      canMoveWorker: true,
    });
  }
  if (!capabilityDescriptor.runtime.compatible) {
    const range = capabilityDescriptor.runtime.compatibleRange;
    const maximum = range.maximumExclusive ? ` and below ${range.maximumExclusive}` : "";
    const actual = capabilityDescriptor.runtime.version
      ? `version ${capabilityDescriptor.runtime.version}`
      : "version could not be verified";
    throw new ProtocolFailure({
      code: "provider_version_incompatible",
      message: `${capabilityDescriptor.runtime.name} ${actual}; this Host requires ${range.minimumInclusive} or newer${maximum}.`,
      retryable: false,
      requiresNewExecution: false,
      requiresUserAction: true,
      canReconstructFromHistory: true,
      canMoveWorker: true,
    });
  }
}

function requireActiveTurn(
  state: ProtocolState,
  commandType: ProviderHostCommand["commandType"],
): NonNullable<ProtocolState["activeTurn"]> {
  if (state.activeTurn) return state.activeTurn;
  throw new ProtocolFailure({
    code: "session_resume_invalid",
    message: `${commandType} requires an active SendTurn command.`,
    retryable: false,
    requiresNewExecution: false,
    requiresUserAction: false,
    canReconstructFromHistory: true,
    canMoveWorker: true,
  });
}

function validateTargetCommandId(value: unknown, activeCommandId: string): void {
  if (value === undefined) return;
  if (typeof value === "string" && value.trim() === activeCommandId) return;
  throw new ProtocolFailure({
    code: "protocol_violation",
    message: "Control command targetCommandId does not match the active SendTurn command.",
    retryable: false,
    requiresNewExecution: false,
    requiresUserAction: false,
    canReconstructFromHistory: true,
    canMoveWorker: false,
  });
}

function unsupportedActiveTurnCommand(commandType: "SteerTurn"): ProtocolFailure {
  return new ProtocolFailure({
    code: "capability_unsupported",
    message: `${commandType} is not supported by the active Provider runtime.`,
    retryable: false,
    requiresNewExecution: false,
    requiresUserAction: true,
    canReconstructFromHistory: true,
    canMoveWorker: true,
  });
}

function validateResolutionCommandPayload(
  payload: Record<string, unknown>,
  commandType: "ResolveApproval" | "ResolveUserInput",
): void {
  requiredString(payload.requestId, `${commandType} requestId`);
  if (!isRecord(payload.resolution)) {
    throw new ProtocolFailure({
      code: "protocol_violation",
      message: `${commandType} resolution must be an object.`,
      retryable: false,
      requiresNewExecution: false,
      requiresUserAction: false,
      canReconstructFromHistory: true,
      canMoveWorker: false,
    });
  }
}

function unsupportedInteractiveCommand(
  commandType: "ResolveApproval" | "ResolveUserInput",
): ProtocolFailure {
  return new ProtocolFailure({
    code: "capability_unsupported",
    message: `${commandType} is not supported by the active Provider runtime.`,
    retryable: false,
    requiresNewExecution: true,
    requiresUserAction: true,
    canReconstructFromHistory: true,
    canMoveWorker: true,
  });
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

function bindRunnerInputGeneration(input: RunnerInput, commandGeneration: number): RunnerInput {
  const inputGeneration = input.execution.generation;
  if (inputGeneration !== undefined && inputGeneration !== commandGeneration) {
    throw new ProtocolFailure({
      code: "protocol_violation",
      message: "runnerInput.execution.generation does not match command.generation.",
      retryable: false,
      requiresNewExecution: true,
      requiresUserAction: false,
      canReconstructFromHistory: true,
      canMoveWorker: true,
    });
  }
  return {
    ...input,
    execution: { ...input.execution, generation: commandGeneration },
  };
}

function readProvider(value: unknown): ProviderHostProviderKind {
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
    commandId: safeWireString(
      candidate.commandId,
      "protocol-command",
    ) as ProviderHostCommand["commandId"],
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
  if (normalized.includes("interrupted")) {
    return errorDetail("interrupted", message, false, false, false, true, true);
  }
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
