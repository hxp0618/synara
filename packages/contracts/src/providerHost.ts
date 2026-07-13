// FILE: providerHost.ts
// Purpose: Defines the schema-only Provider Host v2 wire and capability contracts.
// Layer: Shared contracts

import { Schema } from "effect";

import { CommandId, IsoDateTime, NonNegativeInt, PositiveInt, TrimmedNonEmptyString } from "./baseSchemas";
import providerCapabilityCatalog from "./providerCapabilityCatalog.json";
import {
  PROVIDER_RUNTIME_EVENT_VERSION,
  ProviderRuntimeEventType,
} from "./providerRuntime";

export const PROVIDER_HOST_PROTOCOL_VERSION = { major: 2, minor: 1 } as const;
export const PROVIDER_HOST_MAX_COMMAND_BYTES = 2 * 1024 * 1024;
export const PROVIDER_HOST_MAX_MESSAGE_BYTES = 1024 * 1024;

export const PROVIDER_HOST_PROVIDER_KINDS = [
  "codex",
  "claudeAgent",
  "cursor",
  "gemini",
  "grok",
  "kilo",
  "opencode",
  "pi",
] as const;

export const ProviderHostProviderKind = Schema.Literals(PROVIDER_HOST_PROVIDER_KINDS);
export type ProviderHostProviderKind = typeof ProviderHostProviderKind.Type;

export const PROVIDER_CAPABILITY_IDS = [
  "discovery",
  "start-session",
  "resume-session",
  "send-turn",
  "steer-turn",
  "interrupt-turn",
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

export const ProviderCapabilityId = Schema.Literals(PROVIDER_CAPABILITY_IDS);
export type ProviderCapabilityId = typeof ProviderCapabilityId.Type;

export const ProviderCapabilitySupport = Schema.Literals([
  "native",
  "emulated",
  "unsupported",
]);
export type ProviderCapabilitySupport = typeof ProviderCapabilitySupport.Type;

export const ProviderSupportTier = Schema.Literals([
  "tier-1",
  "tier-2",
  "experimental",
  "local-only",
]);
export type ProviderSupportTier = typeof ProviderSupportTier.Type;

export const ProviderHostProtocolVersion = Schema.Struct({
  major: PositiveInt,
  minor: NonNegativeInt,
});
export type ProviderHostProtocolVersion = typeof ProviderHostProtocolVersion.Type;

export const ProviderCapabilityMap = Schema.Record(
  ProviderCapabilityId,
  ProviderCapabilitySupport,
)
  .check(Schema.isMinProperties(PROVIDER_CAPABILITY_IDS.length))
  .check(Schema.isMaxProperties(PROVIDER_CAPABILITY_IDS.length));
export type ProviderCapabilityMap = typeof ProviderCapabilityMap.Type;

export const ProviderRuntimeKind = Schema.Literals(["cli", "sdk", "local"]);
export type ProviderRuntimeKind = typeof ProviderRuntimeKind.Type;

export const ProviderRuntimeVersionSource = Schema.Literals(["probe", "package", "build"]);
export type ProviderRuntimeVersionSource = typeof ProviderRuntimeVersionSource.Type;

export const ProviderRuntimeCompatibleRange = Schema.Struct({
  minimumInclusive: TrimmedNonEmptyString,
  maximumExclusive: Schema.optional(TrimmedNonEmptyString),
});
export type ProviderRuntimeCompatibleRange = typeof ProviderRuntimeCompatibleRange.Type;

export const ProviderRuntimeDescriptor = Schema.Struct({
  kind: ProviderRuntimeKind,
  name: TrimmedNonEmptyString,
  version: Schema.optional(TrimmedNonEmptyString),
  available: Schema.Boolean,
  versionSource: ProviderRuntimeVersionSource,
  compatibleRange: ProviderRuntimeCompatibleRange,
  compatible: Schema.Boolean,
});
export type ProviderRuntimeDescriptor = typeof ProviderRuntimeDescriptor.Type;

export const ProviderReleasePolicy = Schema.Struct({
  requiresExplicitEnablement: Schema.Boolean,
  enabled: Schema.Boolean,
});
export type ProviderReleasePolicy = typeof ProviderReleasePolicy.Type;

export const ProviderCapabilityDescriptor = Schema.Struct({
  provider: ProviderHostProviderKind,
  supportTier: ProviderSupportTier,
  adapterVersion: TrimmedNonEmptyString,
  providerCliVersion: Schema.optional(TrimmedNonEmptyString),
  runtime: ProviderRuntimeDescriptor,
  releasePolicy: ProviderReleasePolicy,
  capabilities: ProviderCapabilityMap,
});
export type ProviderCapabilityDescriptor = typeof ProviderCapabilityDescriptor.Type;

export type ProviderCapabilityCatalogRuntimePolicy = {
  readonly kind: ProviderRuntimeKind;
  readonly name: string;
  readonly versionSource: ProviderRuntimeVersionSource;
  readonly compatibleRange: ProviderRuntimeCompatibleRange;
};

export type ProviderCapabilityCatalogEntry = {
  readonly provider: ProviderHostProviderKind;
  readonly supportTier: ProviderSupportTier;
  readonly adapterVersion: string;
  readonly runtimePolicy: ProviderCapabilityCatalogRuntimePolicy;
  readonly capabilities: ProviderCapabilityMap;
};

export type ProviderCapabilityCatalog = {
  readonly version: 1;
  readonly capabilityIds: ReadonlyArray<ProviderCapabilityId>;
  readonly providers: ReadonlyArray<ProviderCapabilityCatalogEntry>;
};

export const PROVIDER_CAPABILITY_CATALOG =
  providerCapabilityCatalog as ProviderCapabilityCatalog;

export const ProviderCredentialDeliveryMode = Schema.Literals(["anonymous-fd"]);
export type ProviderCredentialDeliveryMode = typeof ProviderCredentialDeliveryMode.Type;

export const ProviderResumeStrategy = Schema.Literals([
  "native-cursor",
  "authoritative-history",
]);
export type ProviderResumeStrategy = typeof ProviderResumeStrategy.Type;

export const ProviderHostDescriptor = Schema.Struct({
  protocolVersion: ProviderHostProtocolVersion,
  hostBuildVersion: TrimmedNonEmptyString,
  capabilityDescriptor: ProviderCapabilityDescriptor,
  maximumCommandBytes: PositiveInt,
  maximumMessageBytes: PositiveInt,
  runtimeEventVersions: Schema.Struct({
    minimum: PositiveInt,
    maximum: PositiveInt,
  }),
  credentialDeliveryModes: Schema.Array(ProviderCredentialDeliveryMode),
  resumeStrategies: Schema.Array(ProviderResumeStrategy),
});
export type ProviderHostDescriptor = typeof ProviderHostDescriptor.Type;

export const PROVIDER_HOST_COMMAND_TYPES = [
  "Describe",
  "StartSession",
  "ResumeSession",
  "SendTurn",
  "SteerTurn",
  "InterruptTurn",
  "ResolveApproval",
  "ResolveUserInput",
  "CompactSession",
  "RollbackSession",
  "ForkSession",
  "StartReview",
  "StopSession",
] as const;

export const ProviderHostCommandType = Schema.Literals(PROVIDER_HOST_COMMAND_TYPES);
export type ProviderHostCommandType = typeof ProviderHostCommandType.Type;

const WireIdentifier = TrimmedNonEmptyString.pipe(Schema.check(Schema.isMaxLength(200)));
const WirePayload = Schema.Record(Schema.String, Schema.Unknown);

export const ProviderHostCommandEnvelope = Schema.Struct({
  requestId: WireIdentifier,
  protocolVersion: ProviderHostProtocolVersion,
  executionId: WireIdentifier,
  generation: PositiveInt,
  commandType: ProviderHostCommandType,
  commandId: CommandId,
  occurredAt: IsoDateTime,
  payload: WirePayload,
});
export type ProviderHostCommandEnvelope = typeof ProviderHostCommandEnvelope.Type;

export const PROVIDER_HOST_ERROR_CODES = [
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

export const ProviderHostErrorCode = Schema.Literals(PROVIDER_HOST_ERROR_CODES);
export type ProviderHostErrorCode = typeof ProviderHostErrorCode.Type;

export const ProviderHostError = Schema.Struct({
  code: ProviderHostErrorCode,
  message: TrimmedNonEmptyString.pipe(Schema.check(Schema.isMaxLength(2_000))),
  retryable: Schema.Boolean,
  requiresNewExecution: Schema.Boolean,
  requiresUserAction: Schema.Boolean,
  canReconstructFromHistory: Schema.Boolean,
  canMoveWorker: Schema.Boolean,
});
export type ProviderHostError = typeof ProviderHostError.Type;

const ProviderHostMessageBase = {
  requestId: WireIdentifier,
  protocolVersion: ProviderHostProtocolVersion,
  executionId: WireIdentifier,
  generation: PositiveInt,
  commandId: CommandId,
  occurredAt: IsoDateTime,
};

const messageWithPayload = <MessageType extends string>(messageType: MessageType) =>
  Schema.Struct({
    ...ProviderHostMessageBase,
    messageType: Schema.Literal(messageType),
    payload: WirePayload,
  });

export const ProviderHostRuntimeEventPayload = Schema.Struct({
  eventVersion: Schema.Literal(PROVIDER_RUNTIME_EVENT_VERSION),
  eventType: ProviderRuntimeEventType,
  payload: WirePayload,
});
export type ProviderHostRuntimeEventPayload = typeof ProviderHostRuntimeEventPayload.Type;

export const ProviderHostEventMessage = Schema.Struct({
  ...ProviderHostMessageBase,
  messageType: Schema.Literal("Event"),
  payload: ProviderHostRuntimeEventPayload,
});
export const ProviderHostInteractionRequestMessage = messageWithPayload("InteractionRequest");
export const ProviderHostArtifactCandidateMessage = messageWithPayload("ArtifactCandidate");
export const ProviderHostCheckpointMessage = messageWithPayload("Checkpoint");
export const ProviderHostResultMessage = messageWithPayload("Result");
export const ProviderHostProgressMessage = messageWithPayload("Progress");
export const ProviderHostErrorMessage = Schema.Struct({
  ...ProviderHostMessageBase,
  messageType: Schema.Literal("Error"),
  error: ProviderHostError,
});

export const ProviderHostMessageEnvelope = Schema.Union([
  ProviderHostEventMessage,
  ProviderHostInteractionRequestMessage,
  ProviderHostArtifactCandidateMessage,
  ProviderHostCheckpointMessage,
  ProviderHostResultMessage,
  ProviderHostErrorMessage,
  ProviderHostProgressMessage,
]);
export type ProviderHostMessageEnvelope = typeof ProviderHostMessageEnvelope.Type;
