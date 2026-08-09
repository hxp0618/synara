import { randomUUID } from "node:crypto";

import {
  CLOUD_AGENT_PROTOCOL_VERSION,
  type CloudAgentCapabilityMap,
  type CloudAgentCommandEnvelope,
  type CloudAgentCommandType,
  type CloudAgentMessageEnvelope,
} from "@synara/cloud-agent-protocol";
import type {
  ProviderApprovalDecision,
  ProviderSessionStartInput,
  ProviderUserInputAnswers,
} from "@synara/contracts";

import { ProviderAdapterValidationError, ProviderAdapterRequestError } from "../Errors.ts";

const PROVIDER = "codex" as const;
const CURSOR_KIND = "synara-cloud-agent-codex" as const;
const CURSOR_VERSION = 1 as const;

export interface CloudAgentCodexCursor {
  readonly kind: typeof CURSOR_KIND;
  readonly version: typeof CURSOR_VERSION;
  readonly providerResumeCursor: string;
}

export interface CloudAgentCodexDescriptor {
  readonly providerKind: "codex";
  readonly runtime: {
    readonly available: boolean;
    readonly compatible: boolean;
    readonly version?: string;
  };
  readonly capabilities: CloudAgentCapabilityMap;
}

const REQUIRED_CAPABILITIES = [
  "start-session",
  "send-turn",
  "interrupt-turn",
  "approval",
  "structured-user-input",
] as const;

export function assertCloudAgentCodexDescriptor(
  value: unknown,
): asserts value is CloudAgentCodexDescriptor {
  if (!isRecord(value) || value.providerKind !== PROVIDER) {
    throw validation("Describe", "runtime descriptor does not describe the Codex provider.");
  }
  if (!isRecord(value.runtime)) {
    throw validation("Describe", "runtime descriptor omitted runtime compatibility metadata.");
  }
  if (value.runtime.available !== true) {
    throw validation("Describe", "Cloud Agent reported that the Codex runtime is unavailable.");
  }
  if (value.runtime.compatible !== true) {
    throw validation("Describe", "Cloud Agent reported an incompatible Codex runtime.");
  }
  if (!isRecord(value.capabilities)) {
    throw validation("Describe", "runtime descriptor omitted its capability map.");
  }
  for (const capability of REQUIRED_CAPABILITIES) {
    if (value.capabilities[capability] === "unsupported") {
      throw validation(
        "Describe",
        `Cloud Agent does not support required capability '${capability}'.`,
      );
    }
    if (
      value.capabilities[capability] !== "native" &&
      value.capabilities[capability] !== "emulated"
    ) {
      throw validation("Describe", `Cloud Agent returned invalid support for '${capability}'.`);
    }
  }
}

export function makeCloudAgentCommand(input: {
  readonly executionId: string;
  readonly generation: number;
  readonly commandType: CloudAgentCommandType;
  readonly payload?: Readonly<Record<string, unknown>>;
  readonly commandId?: string;
}): CloudAgentCommandEnvelope {
  const commandId = input.commandId ?? randomUUID();
  return {
    requestId: commandId,
    protocolVersion: CLOUD_AGENT_PROTOCOL_VERSION,
    executionId: input.executionId,
    generation: input.generation,
    commandType: input.commandType,
    commandId,
    occurredAt: new Date().toISOString(),
    payload: input.payload ?? {},
  };
}

export function cloudAgentRunnerInput(input: ProviderSessionStartInput): Record<string, unknown> {
  if (input.provider !== undefined && input.provider !== PROVIDER) {
    throw validation(
      "startSession",
      `expected provider '${PROVIDER}', received '${input.provider}'.`,
    );
  }
  if (input.runtimeMode === "auto") {
    throw validation("startSession", "runtime mode 'auto' is not representable by Cloud Agent v2.");
  }
  if (input.modelSelection && input.modelSelection.provider !== PROVIDER) {
    throw validation(
      "startSession",
      `model selection belongs to provider '${input.modelSelection.provider}'.`,
    );
  }
  const modelOptions = input.modelSelection?.options;
  if (modelOptions && Object.values(modelOptions).some((value) => value !== undefined)) {
    throw validation(
      "startSession",
      "the selected Codex model options are not representable by Cloud Agent v2.",
    );
  }
  const codexOptions = input.providerOptions?.codex;
  if (codexOptions && Object.values(codexOptions).some((value) => value !== undefined)) {
    throw validation(
      "startSession",
      "Codex binary/home overrides are not representable by the selected Cloud Agent runtime.",
    );
  }
  if (input.approvalPolicy !== undefined || input.sandboxMode !== undefined) {
    throw validation(
      "startSession",
      "explicit approvalPolicy/sandboxMode are not representable by Cloud Agent v2.",
    );
  }
  return {
    execution: { id: input.threadId },
    workload: {
      provider: PROVIDER,
      inputText: "",
      runtimeMode: input.runtimeMode,
      ...(input.modelSelection ? { model: input.modelSelection.model } : {}),
    },
    workspaceDirectory: input.cwd ?? process.cwd(),
  };
}

export function decodeCloudAgentCursor(value: unknown): CloudAgentCodexCursor | undefined {
  if (value === undefined || value === null) return undefined;
  if (!isRecord(value)) {
    throw validation("startSession", "resume cursor was not created by CloudAgentCodexAdapter.");
  }
  if (
    value.kind !== CURSOR_KIND ||
    value.version !== CURSOR_VERSION ||
    typeof value.providerResumeCursor !== "string" ||
    !value.providerResumeCursor.trim()
  ) {
    throw validation("startSession", "resume cursor was not created by CloudAgentCodexAdapter.");
  }
  return {
    kind: CURSOR_KIND,
    version: CURSOR_VERSION,
    providerResumeCursor: value.providerResumeCursor,
  };
}

export function encodeCloudAgentCursor(
  providerResumeCursor: string | undefined,
): CloudAgentCodexCursor | undefined {
  const normalized = providerResumeCursor?.trim();
  return normalized
    ? { kind: CURSOR_KIND, version: CURSOR_VERSION, providerResumeCursor: normalized }
    : undefined;
}

export function providerCursorFromTerminal(message: CloudAgentMessageEnvelope): string | undefined {
  if (message.messageType !== "Result") return undefined;
  const value = message.payload.providerResumeCursor;
  return typeof value === "string" && value.trim() ? value.trim() : undefined;
}

export function assertCloudAgentResult(
  operation: string,
  message: CloudAgentMessageEnvelope,
): void {
  if (message.messageType === "Result") return;
  if (message.messageType === "Error") {
    throw new ProviderAdapterRequestError({
      provider: PROVIDER,
      method: operation,
      detail: `${message.error.code}: ${message.error.message}`,
    });
  }
  throw new ProviderAdapterRequestError({
    provider: PROVIDER,
    method: operation,
    detail: `expected terminal Result, received ${message.messageType}.`,
  });
}

export function approvalResolution(
  requestId: string,
  decision: ProviderApprovalDecision,
): Readonly<Record<string, unknown>> {
  return { requestId, resolution: { decision } };
}

export function userInputResolution(
  requestId: string,
  answers: ProviderUserInputAnswers,
): Readonly<Record<string, unknown>> {
  return { requestId, resolution: { answers } };
}

function validation(operation: string, issue: string): ProviderAdapterValidationError {
  return new ProviderAdapterValidationError({ provider: PROVIDER, operation, issue });
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
