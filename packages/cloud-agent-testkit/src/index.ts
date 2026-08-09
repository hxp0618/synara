import {
  CLOUD_AGENT_CAPABILITY_IDS,
  CLOUD_AGENT_PROTOCOL_VERSION,
  CLOUD_AGENT_RUNTIME_EVENT_TYPES,
  CLOUD_AGENT_RUNTIME_EVENT_VERSION,
  type CloudAgentCommandEnvelope,
  type CloudAgentMessageEnvelope,
} from "@synara/cloud-agent-protocol";
import {
  CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
  type CloudAgentProviderDescriptor,
} from "@synara/cloud-agent-provider-api";

export function assertCloudAgentDescriptor(descriptor: CloudAgentProviderDescriptor): void {
  if (descriptor.abiVersion !== CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION) {
    throw new Error(`Unexpected Provider Plugin ABI ${descriptor.abiVersion}.`);
  }
  if (!/^[a-zA-Z][a-zA-Z0-9_-]{0,63}$/u.test(descriptor.providerKind)) {
    throw new Error("Provider kind is not a portable slug.");
  }
  const capabilityKeys = Object.keys(descriptor.capabilities).toSorted();
  const expected = [...CLOUD_AGENT_CAPABILITY_IDS].toSorted();
  if (
    capabilityKeys.length !== expected.length ||
    capabilityKeys.some((key, i) => key !== expected[i])
  ) {
    throw new Error("Provider descriptor does not define the complete capability map.");
  }
  if (!descriptor.runtime.compatibleRange.minimumInclusive.trim()) {
    throw new Error("Provider descriptor omitted its minimum compatible upstream version.");
  }
  if (!descriptor.displayName.trim() || !descriptor.adapterVersion.trim()) {
    throw new Error("Provider descriptor omitted displayName or adapterVersion.");
  }
  for (const capability of CLOUD_AGENT_CAPABILITY_IDS) {
    const support = descriptor.capabilities[capability];
    if (support !== "native" && support !== "emulated" && support !== "unsupported") {
      throw new Error(`Provider descriptor has invalid support '${support}' for ${capability}.`);
    }
  }
  if (!descriptor.runtime.name.trim()) {
    throw new Error("Provider descriptor omitted its upstream Runtime name.");
  }
  const tasks = descriptor.textGenerationTasks ?? [];
  if (new Set(tasks).size !== tasks.length) {
    throw new Error("Provider descriptor repeats a text-generation task.");
  }
}

export function cloudAgentCommand(input: {
  readonly commandType: CloudAgentCommandEnvelope["commandType"];
  readonly commandId: string;
  readonly payload?: Readonly<Record<string, unknown>>;
  readonly executionId?: string;
  readonly generation?: number;
  readonly occurredAt?: string;
}): CloudAgentCommandEnvelope {
  return {
    requestId: `request:${input.commandId}`,
    protocolVersion: CLOUD_AGENT_PROTOCOL_VERSION,
    executionId: input.executionId ?? "test-execution",
    generation: input.generation ?? 1,
    commandType: input.commandType,
    commandId: input.commandId,
    occurredAt: input.occurredAt ?? "2026-08-09T00:00:00.000Z",
    payload: input.payload ?? {},
  };
}

export function assertTerminalCorrelation(
  command: CloudAgentCommandEnvelope,
  terminal: CloudAgentMessageEnvelope,
): void {
  if (terminal.messageType !== "Result" && terminal.messageType !== "Error") {
    throw new Error(`Expected terminal Result/Error, received ${terminal.messageType}.`);
  }
  for (const [field, expected, actual] of [
    ["requestId", command.requestId, terminal.requestId],
    ["executionId", command.executionId, terminal.executionId],
    ["generation", command.generation, terminal.generation],
    ["commandId", command.commandId, terminal.commandId],
  ] as const) {
    if (expected !== actual) throw new Error(`Terminal ${field} does not match its command.`);
  }
  assertMessageMetadata(command, terminal);
}

/**
 * Validates a complete multiplexed transcript, including the ordering rule
 * that no frame may follow a command's terminal receipt.
 */
export function assertCloudAgentTranscript(
  commands: ReadonlyArray<CloudAgentCommandEnvelope>,
  messages: ReadonlyArray<CloudAgentMessageEnvelope>,
): void {
  const commandsById = new Map(commands.map((command) => [command.commandId, command] as const));
  if (commandsById.size !== commands.length) throw new Error("Transcript repeats a commandId.");
  const terminalCommandIds = new Set<string>();

  for (const message of messages) {
    const command = commandsById.get(message.commandId);
    if (!command) throw new Error(`Transcript contains unknown command ${message.commandId}.`);
    if (terminalCommandIds.has(message.commandId)) {
      throw new Error(`Transcript contains a frame after terminal command ${message.commandId}.`);
    }
    assertMessageMetadata(command, message);
    if (message.messageType === "Event") {
      if (message.payload.eventVersion !== CLOUD_AGENT_RUNTIME_EVENT_VERSION) {
        throw new Error(`Runtime Event ${message.commandId} has an unsupported eventVersion.`);
      }
      if (
        !(CLOUD_AGENT_RUNTIME_EVENT_TYPES as readonly string[]).includes(message.payload.eventType)
      ) {
        throw new Error(`Runtime Event ${message.commandId} has an unknown eventType.`);
      }
    }
    if (message.messageType === "Result" || message.messageType === "Error") {
      terminalCommandIds.add(message.commandId);
    }
  }

  for (const command of commands) {
    if (!terminalCommandIds.has(command.commandId)) {
      throw new Error(`Transcript omitted terminal command ${command.commandId}.`);
    }
  }
}

function assertMessageMetadata(
  command: CloudAgentCommandEnvelope,
  message: CloudAgentMessageEnvelope,
): void {
  if (
    message.protocolVersion.major !== command.protocolVersion.major ||
    message.protocolVersion.minor < command.protocolVersion.minor
  ) {
    throw new Error("Message Protocol version is incompatible with its command.");
  }
  if (Number.isNaN(Date.parse(message.occurredAt))) {
    throw new Error("Message occurredAt is not an ISO-compatible timestamp.");
  }
  for (const [field, expected, actual] of [
    ["requestId", command.requestId, message.requestId],
    ["executionId", command.executionId, message.executionId],
    ["generation", command.generation, message.generation],
    ["commandId", command.commandId, message.commandId],
  ] as const) {
    if (expected !== actual) throw new Error(`Message ${field} does not match its command.`);
  }
}
