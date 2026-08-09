import type { CLOUD_AGENT_RUNTIME_EVENT_VERSION, CloudAgentRuntimeEventType } from "./runtimeEvent";

export const CLOUD_AGENT_PROTOCOL_VERSION = { major: 2, minor: 3 } as const;
export const CLOUD_AGENT_MAX_COMMAND_BYTES = 2 * 1024 * 1024;
export const CLOUD_AGENT_MAX_MESSAGE_BYTES = 1024 * 1024;

export const CLOUD_AGENT_CAPABILITY_IDS = [
  "discovery",
  "start-session",
  "resume-session",
  "send-turn",
  "steer-turn",
  "interrupt-turn",
  "suspend-active-turn",
  "approval",
  "structured-user-input",
  "plan-mode",
  "review",
  "compact",
  "rollback",
  "fork",
  "read-history",
  "model-list",
  "model-switch",
  "skill-discovery",
  "skill-mentions",
  "plugin-discovery",
  "plugin-mentions",
  "native-commands",
  "tool-events",
  "diff-events",
  "usage-events",
  "checkpoint",
  "credential-injection",
  "authoritative-history-reconstruction",
  "worker-migration",
] as const;

export type CloudAgentCapabilityId = (typeof CLOUD_AGENT_CAPABILITY_IDS)[number];
export type CloudAgentCapabilitySupport = "native" | "emulated" | "unsupported";
export type CloudAgentCapabilityMap = Readonly<
  Record<CloudAgentCapabilityId, CloudAgentCapabilitySupport>
>;

export const CLOUD_AGENT_COMMAND_TYPES = [
  "Describe",
  "StartSession",
  "ResumeSession",
  "SendTurn",
  "SteerTurn",
  "InterruptTurn",
  "SuspendTurn",
  "ResolveApproval",
  "ResolveUserInput",
  "CompactSession",
  "RollbackSession",
  "ForkSession",
  "StartReview",
  "GenerateText",
  "StopSession",
] as const;

export type CloudAgentCommandType = (typeof CLOUD_AGENT_COMMAND_TYPES)[number];

export const CLOUD_AGENT_TEXT_GENERATION_TASKS = [
  "thread-title",
  "branch-name",
  "commit-message",
  "pr-content",
] as const;
export type CloudAgentTextGenerationTask = (typeof CLOUD_AGENT_TEXT_GENERATION_TASKS)[number];

export const CLOUD_AGENT_ERROR_CODES = [
  "provider_not_installed",
  "provider_version_incompatible",
  "capability_unsupported",
  "credential_missing",
  "credential_invalid",
  "authentication_required",
  "session_resume_invalid",
  "session_resume_expired",
  "provider_rate_limited",
  "provider_unavailable",
  "workspace_invalid",
  "protocol_violation",
  "cancelled",
  "interrupted",
  "internal_error",
] as const;

export type CloudAgentErrorCode = (typeof CLOUD_AGENT_ERROR_CODES)[number];

export interface CloudAgentProtocolVersion {
  readonly major: number;
  readonly minor: number;
}

export interface CloudAgentCommandEnvelope {
  readonly requestId: string;
  readonly protocolVersion: CloudAgentProtocolVersion;
  readonly executionId: string;
  readonly generation: number;
  readonly commandType: CloudAgentCommandType;
  readonly commandId: string;
  readonly occurredAt: string;
  readonly payload: Readonly<Record<string, unknown>>;
}

export interface CloudAgentError {
  readonly code: CloudAgentErrorCode;
  readonly message: string;
  readonly retryable: boolean;
  readonly requiresNewExecution: boolean;
  readonly requiresUserAction: boolean;
  readonly canReconstructFromHistory: boolean;
  readonly canMoveWorker: boolean;
}

interface CloudAgentMessageBase {
  readonly requestId: string;
  readonly protocolVersion: CloudAgentProtocolVersion;
  readonly executionId: string;
  readonly generation: number;
  readonly commandId: string;
  readonly occurredAt: string;
}

export type CloudAgentPayloadMessageType =
  | "InteractionRequest"
  | "ArtifactCandidate"
  | "Checkpoint"
  | "Result"
  | "Progress";

export interface CloudAgentPayloadMessage extends CloudAgentMessageBase {
  readonly messageType: CloudAgentPayloadMessageType;
  readonly payload: Readonly<Record<string, unknown>>;
}

export interface CloudAgentEventMessage extends CloudAgentMessageBase {
  readonly messageType: "Event";
  readonly payload: {
    readonly eventVersion: typeof CLOUD_AGENT_RUNTIME_EVENT_VERSION;
    readonly eventType: CloudAgentRuntimeEventType;
    readonly payload: Readonly<Record<string, unknown>>;
  };
}

export interface CloudAgentErrorMessage extends CloudAgentMessageBase {
  readonly messageType: "Error";
  readonly error: CloudAgentError;
}

export type CloudAgentMessageEnvelope =
  | CloudAgentPayloadMessage
  | CloudAgentEventMessage
  | CloudAgentErrorMessage;
