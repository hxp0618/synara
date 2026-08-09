import { randomUUID } from "node:crypto";

import type { CloudAgentMessageEnvelope } from "@synara/cloud-agent-protocol";
import {
  ProviderRuntimeEvent,
  type ProviderRuntimeEvent as SynaraRuntimeEvent,
  type ThreadId,
  type TurnId,
} from "@synara/contracts";
import { Schema } from "effect";

import { ProviderAdapterValidationError } from "../Errors.ts";

const PROVIDER = "codex" as const;

export interface CloudAgentProjectionContext {
  readonly threadId: ThreadId;
  readonly lifecycleGeneration?: string;
  readonly turnId?: TurnId;
}

export function projectCloudAgentMessage(
  message: CloudAgentMessageEnvelope,
  context: CloudAgentProjectionContext,
): SynaraRuntimeEvent | undefined {
  if (message.messageType === "ArtifactCandidate" || message.messageType === "Checkpoint") {
    return undefined;
  }
  if (message.messageType === "InteractionRequest") {
    return projectInteraction(message.payload, context, message.occurredAt);
  }
  if (message.messageType !== "Event") return undefined;
  const payload = message.payload;
  if (payload.eventVersion !== 2 || typeof payload.eventType !== "string") {
    throw validation("projectEvent", "runtime event did not use Runtime Event v2.");
  }
  return decodeRuntimeEvent({
    eventId: randomUUID(),
    provider: PROVIDER,
    threadId: context.threadId,
    createdAt: message.occurredAt,
    ...(context.turnId ? { turnId: context.turnId } : {}),
    ...(context.lifecycleGeneration ? { lifecycleGeneration: context.lifecycleGeneration } : {}),
    type: payload.eventType,
    payload: payload.payload,
  });
}

function projectInteraction(
  payload: Readonly<Record<string, unknown>>,
  context: CloudAgentProjectionContext,
  createdAt: string,
): SynaraRuntimeEvent {
  const requestId = nonEmptyString(payload.requestId, "interaction requestId");
  const interactionType = payload.interactionType;
  const common = {
    eventId: randomUUID(),
    provider: PROVIDER,
    threadId: context.threadId,
    createdAt,
    ...(context.turnId ? { turnId: context.turnId } : {}),
    requestId,
    ...(context.lifecycleGeneration ? { lifecycleGeneration: context.lifecycleGeneration } : {}),
    providerRefs: { providerRequestId: requestId },
  };
  if (interactionType === "approval") {
    const requestKind = payload.requestKind;
    const requestType =
      requestKind === "command"
        ? "command_execution_approval"
        : requestKind === "file-read"
          ? "file_read_approval"
          : requestKind === "file-change"
            ? "file_change_approval"
            : requestKind === "permissions"
              ? "permissions_approval"
              : undefined;
    if (!requestType) throw validation("projectInteraction", "approval requestKind is invalid.");
    return decodeRuntimeEvent({
      ...common,
      type: "request.opened",
      payload: {
        requestType,
        ...(typeof payload.command === "string" && payload.command.trim()
          ? { detail: payload.command.trim() }
          : {}),
        args: payload,
      },
    });
  }
  if (interactionType === "user-input") {
    if (!Array.isArray(payload.questions)) {
      throw validation("projectInteraction", "user-input questions are invalid.");
    }
    const questions = payload.questions.map((question) => {
      if (!isRecord(question)) {
        throw validation("projectInteraction", "user-input question is invalid.");
      }
      const options = Array.isArray(question.options)
        ? question.options.map((option) => {
            if (!isRecord(option)) {
              throw validation("projectInteraction", "user-input option is invalid.");
            }
            return {
              label: nonEmptyString(option.label, "user-input option label"),
              description: nonEmptyString(option.description, "user-input option description"),
            };
          })
        : [];
      return {
        id: nonEmptyString(question.id, "user-input question id"),
        header: nonEmptyString(question.header, "user-input question header"),
        question: nonEmptyString(question.question, "user-input question text"),
        options,
        multiSelect: question.multiSelect === true,
      };
    });
    return decodeRuntimeEvent({ ...common, type: "user-input.requested", payload: { questions } });
  }
  throw validation("projectInteraction", "interactionType is unsupported.");
}

function decodeRuntimeEvent(value: unknown): SynaraRuntimeEvent {
  try {
    return Schema.decodeUnknownSync(ProviderRuntimeEvent)(value);
  } catch (cause) {
    throw new ProviderAdapterValidationError({
      provider: PROVIDER,
      operation: "projectEvent",
      issue: "Cloud Agent event cannot be represented by Synara's RuntimeEvent contract.",
      cause,
    });
  }
}

function nonEmptyString(value: unknown, label: string): string {
  if (typeof value === "string" && value.trim()) return value.trim();
  throw validation("projectInteraction", `${label} is required.`);
}

function validation(operation: string, issue: string): ProviderAdapterValidationError {
  return new ProviderAdapterValidationError({ provider: PROVIDER, operation, issue });
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
