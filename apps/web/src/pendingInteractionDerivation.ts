import {
  ApprovalRequestId,
  type OrchestrationPendingInteraction,
  type OrchestrationThreadActivity,
  type UserInputQuestion,
} from "@synara/contracts";
import {
  approvalRequestKindFromRequestType,
  pendingRequestInstanceKey,
  type ApprovalRequestKind,
} from "@synara/shared/threadSummary";

import { isStalePendingRequestFailureDetail } from "./lib/pendingInteraction";
import { orderedActivities } from "./workLog";

export interface PendingApproval {
  interactionId?: string;
  requestKey?: string;
  requestId: ApprovalRequestId;
  lifecycleGeneration?: string;
  executionId?: string;
  requestKind: ApprovalRequestKind;
  createdAt: string;
  detail?: string;
}

export interface PendingUserInput {
  interactionId?: string;
  requestKey?: string;
  requestId: ApprovalRequestId;
  lifecycleGeneration?: string;
  executionId?: string;
  createdAt: string;
  questions: ReadonlyArray<UserInputQuestion>;
}

type PendingInteractionKind = OrchestrationPendingInteraction["interactionKind"];

interface PendingInteractionReplay<T extends { requestId: ApprovalRequestId }> {
  interactionKind: PendingInteractionKind;
  requestedActivityKinds: ReadonlySet<string>;
  resolvedActivityKinds: ReadonlySet<string>;
  responseFailedActivityKind: string;
  parseRequested: (input: {
    activity: OrchestrationThreadActivity;
    payload: Record<string, unknown> | null;
    requestId: ApprovalRequestId;
    lifecycleGeneration: string | undefined;
    interactionId: string | undefined;
  }) => T | null;
}

function activityPayload(activity: OrchestrationThreadActivity): Record<string, unknown> | null {
  return activity.payload && typeof activity.payload === "object"
    ? (activity.payload as Record<string, unknown>)
    : null;
}

function activityLifecycleGeneration(payload: Record<string, unknown> | null): string | undefined {
  const generation = payload?.lifecycleGeneration;
  return typeof generation === "string" && generation.length > 0 ? generation : undefined;
}

function activityInteractionId(payload: Record<string, unknown> | null): string | undefined {
  const interactionId = payload?.interactionId;
  return typeof interactionId === "string" && interactionId.trim().length > 0
    ? interactionId.trim()
    : undefined;
}

function durableInteractionRequestKey(interactionId: string): string {
  return `interaction:${interactionId}`;
}

function activityExecutionId(payload: Record<string, unknown> | null): string | undefined {
  const executionId = payload?.executionId;
  return typeof executionId === "string" && executionId.length > 0 ? executionId : undefined;
}

function deletePendingInteraction<
  T extends { interactionId?: string; requestId: ApprovalRequestId },
>(
  openByInstance: Map<string, T>,
  requestId: ApprovalRequestId,
  lifecycleGeneration: string | undefined,
  interactionId: string | undefined,
): void {
  if (interactionId !== undefined) {
    openByInstance.delete(durableInteractionRequestKey(interactionId));
    return;
  }
  if (lifecycleGeneration !== undefined) {
    openByInstance.delete(pendingRequestInstanceKey(requestId, lifecycleGeneration));
    return;
  }
  for (const [key, pending] of openByInstance) {
    if (pending.requestId === requestId) openByInstance.delete(key);
  }
}

function replacePendingInteraction<
  T extends { interactionId?: string; requestId: ApprovalRequestId; requestKey?: string },
>(openByInstance: Map<string, T>, pending: T, lifecycleGeneration: string | undefined): void {
  if (pending.interactionId !== undefined) {
    openByInstance.set(
      pending.requestKey ?? durableInteractionRequestKey(pending.interactionId),
      pending,
    );
    return;
  }
  for (const [key, open] of openByInstance) {
    if (open.interactionId === undefined && open.requestId === pending.requestId) {
      openByInstance.delete(key);
    }
  }
  openByInstance.set(pendingRequestInstanceKey(pending.requestId, lifecycleGeneration), pending);
}

function retainActionableSettlements<
  T extends { interactionId?: string; requestId: ApprovalRequestId },
>(
  openByInstance: Map<string, T>,
  settlements: ReadonlyArray<OrchestrationPendingInteraction> | undefined,
  interactionKind: PendingInteractionKind,
): void {
  if (settlements === undefined) {
    return;
  }
  const actionableKeys = new Set(
    settlements
      .filter(
        (settlement) =>
          settlement.interactionKind === interactionKind &&
          (settlement.status === "pending" || settlement.status === "retryable"),
      )
      .flatMap((settlement) => {
        const lifecycleGeneration = settlement.lifecycleGeneration ?? undefined;
        return [
          pendingRequestInstanceKey(settlement.requestId, lifecycleGeneration),
          ...(lifecycleGeneration !== undefined
            ? [durableInteractionRequestKey(lifecycleGeneration)]
            : []),
        ];
      }),
  );
  for (const key of openByInstance.keys()) {
    if (!actionableKeys.has(key)) {
      openByInstance.delete(key);
    }
  }
}

function replayPendingInteractions<
  T extends {
    interactionId?: string;
    requestId: ApprovalRequestId;
    requestKey?: string;
    createdAt: string;
  },
>(
  activities: ReadonlyArray<OrchestrationThreadActivity>,
  settlements: ReadonlyArray<OrchestrationPendingInteraction> | undefined,
  replay: PendingInteractionReplay<T>,
): T[] {
  const openByInstance = new Map<string, T>();

  for (const activity of orderedActivities(activities)) {
    const payload = activityPayload(activity);
    const requestId =
      typeof payload?.requestId === "string"
        ? ApprovalRequestId.makeUnsafe(payload.requestId)
        : null;
    if (!requestId) {
      continue;
    }

    const lifecycleGeneration = activityLifecycleGeneration(payload);
    const interactionId = activityInteractionId(payload);
    if (replay.requestedActivityKinds.has(activity.kind)) {
      const pending = replay.parseRequested({
        activity,
        payload,
        requestId,
        lifecycleGeneration,
        interactionId,
      });
      if (pending) {
        replacePendingInteraction(openByInstance, pending, lifecycleGeneration);
      }
      continue;
    }

    if (replay.resolvedActivityKinds.has(activity.kind)) {
      deletePendingInteraction(openByInstance, requestId, lifecycleGeneration, interactionId);
      continue;
    }

    const detail = typeof payload?.detail === "string" ? payload.detail : undefined;
    if (
      activity.kind === replay.responseFailedActivityKind &&
      isStalePendingRequestFailureDetail(detail)
    ) {
      deletePendingInteraction(openByInstance, requestId, lifecycleGeneration, interactionId);
    }
  }

  retainActionableSettlements(openByInstance, settlements, replay.interactionKind);
  return [...openByInstance.values()].toSorted((left, right) =>
    left.createdAt.localeCompare(right.createdAt),
  );
}

function parseUserInputQuestions(
  payload: Record<string, unknown> | null,
): ReadonlyArray<UserInputQuestion> | null {
  const questions = payload?.questions;
  if (!Array.isArray(questions)) {
    return null;
  }
  const parsed = questions
    .map<UserInputQuestion | null>((entry) => {
      if (!entry || typeof entry !== "object") return null;
      const question = entry as Record<string, unknown>;
      if (
        typeof question.id !== "string" ||
        typeof question.header !== "string" ||
        typeof question.question !== "string" ||
        !Array.isArray(question.options)
      ) {
        return null;
      }
      const options = question.options
        .map<UserInputQuestion["options"][number] | null>((option) => {
          if (!option || typeof option !== "object") return null;
          const optionRecord = option as Record<string, unknown>;
          if (
            typeof optionRecord.label !== "string" ||
            typeof optionRecord.description !== "string"
          ) {
            return null;
          }
          return {
            label: optionRecord.label,
            description: optionRecord.description,
          };
        })
        .filter((option): option is UserInputQuestion["options"][number] => option !== null);
      return {
        id: question.id,
        header: question.header,
        question: question.question,
        options,
        ...(question.multiSelect === true ? { multiSelect: true } : {}),
      };
    })
    .filter((question): question is UserInputQuestion => question !== null);
  return parsed.length > 0 ? parsed : null;
}

export function derivePendingApprovals(
  activities: ReadonlyArray<OrchestrationThreadActivity>,
  settlements?: ReadonlyArray<OrchestrationPendingInteraction>,
): PendingApproval[] {
  return replayPendingInteractions(activities, settlements, {
    interactionKind: "approval",
    requestedActivityKinds: new Set(["approval.requested", "request.opened"]),
    resolvedActivityKinds: new Set(["approval.resolved", "request.resolved"]),
    responseFailedActivityKind: "provider.approval.respond.failed",
    parseRequested: ({ activity, payload, requestId, lifecycleGeneration, interactionId }) => {
      const executionId = activityExecutionId(payload);
      const requestKind =
        payload?.requestKind === "command" ||
        payload?.requestKind === "file-read" ||
        payload?.requestKind === "file-change" ||
        payload?.requestKind === "network" ||
        payload?.requestKind === "tool"
          ? payload.requestKind
          : approvalRequestKindFromRequestType(payload?.requestType);
      if (!requestKind) {
        return null;
      }
      const detail = typeof payload?.detail === "string" ? payload.detail : undefined;
      return {
        ...(interactionId !== undefined
          ? {
              interactionId,
              requestKey: durableInteractionRequestKey(interactionId),
            }
          : {}),
        requestId,
        ...(lifecycleGeneration !== undefined ? { lifecycleGeneration } : {}),
        ...(executionId !== undefined ? { executionId } : {}),
        requestKind,
        createdAt: activity.createdAt,
        ...(detail ? { detail } : {}),
      };
    },
  });
}

export function derivePendingUserInputs(
  activities: ReadonlyArray<OrchestrationThreadActivity>,
  settlements?: ReadonlyArray<OrchestrationPendingInteraction>,
): PendingUserInput[] {
  return replayPendingInteractions(activities, settlements, {
    interactionKind: "userInput",
    requestedActivityKinds: new Set(["user-input.requested"]),
    resolvedActivityKinds: new Set(["user-input.resolved"]),
    responseFailedActivityKind: "provider.user-input.respond.failed",
    parseRequested: ({ activity, payload, requestId, lifecycleGeneration, interactionId }) => {
      const executionId = activityExecutionId(payload);
      const questions = parseUserInputQuestions(payload);
      if (!questions) {
        return null;
      }
      return {
        ...(interactionId !== undefined
          ? {
              interactionId,
              requestKey: durableInteractionRequestKey(interactionId),
            }
          : {}),
        requestId,
        ...(lifecycleGeneration !== undefined ? { lifecycleGeneration } : {}),
        ...(executionId !== undefined ? { executionId } : {}),
        createdAt: activity.createdAt,
        questions,
      };
    },
  });
}
