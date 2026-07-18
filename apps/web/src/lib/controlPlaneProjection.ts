import {
  EventId,
  MessageId,
  ProjectId,
  ThreadId,
  TurnId,
  type OrchestrationLatestTurn,
  type OrchestrationSessionStatus,
  type OrchestrationThreadActivity,
  type ProviderInteractionMode,
  type ProviderKind,
  type RuntimeMode,
} from "@synara/contracts";
import { getDefaultModel } from "@synara/shared/model";

import type { Project, Thread, ThreadSession } from "../types";
import type {
  ControlPlaneAgentSession,
  ControlPlaneProject,
  ControlPlaneSessionEvent,
} from "./controlPlaneClient";

export type ControlPlaneStreamStatus =
  | "idle"
  | "catching-up"
  | "connecting"
  | "live"
  | "reconnecting"
  | "error";

export type ControlPlaneSessionProjection = {
  session: ControlPlaneAgentSession;
  lastAppliedSequence: number;
  durableLastSequence: number;
  streamStatus: ControlPlaneStreamStatus;
  executionTurnIds: Readonly<Record<string, TurnId>>;
  messages: Thread["messages"];
  activities: Thread["activities"];
  latestTurn: OrchestrationLatestTurn | null;
  latestTurnKind: "message" | "compact" | "review" | "rollback" | "fork" | null;
  forkSourceSessionId: string | null;
  forkSourceEventSequence: number | null;
  runtimeMode: RuntimeMode;
  interactionMode: ProviderInteractionMode;
  orchestrationStatus: OrchestrationSessionStatus;
  error: string | null;
};

export type ControlPlaneProjectionApplyResult = {
  projection: ControlPlaneSessionProjection;
  gap: { afterSequence: number; receivedSequence: number } | null;
};

const PROVIDERS = new Set<ProviderKind>([
  "codex",
  "claudeAgent",
  "cursor",
  "antigravity",
  "grok",
  "droid",
  "kilo",
  "opencode",
  "pi",
]);

const WORKSPACE_CHECKPOINT_UNCONFIRMED_RISK = "data-loss-risk=workspace-checkpoint-unconfirmed";
const TERMINAL_FAILURE_LABELS: Readonly<Record<string, string>> = {
  exit: "exit failure",
  signal: "signal failure",
  timeout: "timed out",
  oom: "out of memory",
  provider_error: "provider error",
};

function providerKind(value: string): ProviderKind {
  if (value === "claude" || value === "claudeagent") return "claudeAgent";
  if (value === "gemini") return "antigravity";
  if (PROVIDERS.has(value as ProviderKind)) return value as ProviderKind;
  throw new Error(`Control Plane Session uses unsupported Provider ${JSON.stringify(value)}.`);
}

function payloadString(event: ControlPlaneSessionEvent, key: string): string | null {
  const value = event.payload[key];
  return typeof value === "string" && value.length > 0 ? value : null;
}

function payloadNumber(event: ControlPlaneSessionEvent, key: string): number | null {
  const value = event.payload[key];
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0 ? value : null;
}

function eventTurnKind(
  event: ControlPlaneSessionEvent,
): "message" | "compact" | "review" | "rollback" | "fork" {
  const value = payloadString(event, "turnKind");
  return value === "compact" || value === "review" || value === "rollback" || value === "fork"
    ? value
    : "message";
}

function rollbackMessages(
  messages: Thread["messages"],
  event: ControlPlaneSessionEvent,
): Thread["messages"] {
  const removedTurnIds = Array.isArray(event.payload.removedTurnIds)
    ? new Set(
        event.payload.removedTurnIds.filter(
          (value): value is string => typeof value === "string" && value.length > 0,
        ),
      )
    : null;
  if (removedTurnIds && removedTurnIds.size > 0) {
    return messages.filter((message) => !message.turnId || !removedTurnIds.has(message.turnId));
  }

  const fromTurnId = payloadString(event, "fromTurnId");
  if (!fromTurnId) return messages;
  const rollbackIndex = messages.findIndex((message) => message.turnId === fromTurnId);
  return rollbackIndex < 0 ? messages : messages.slice(0, rollbackIndex);
}

function recordValue(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function terminalData(event: ControlPlaneSessionEvent): Record<string, unknown> | null {
  return recordValue(recordValue(event.payload.data)?.terminal);
}

function recordString(value: Record<string, unknown>, key: string): string | null {
  const field = value[key];
  return typeof field === "string" && field.length > 0 ? field : null;
}

function recordNumber(value: Record<string, unknown>, key: string): number | null {
  const field = value[key];
  return typeof field === "number" && Number.isFinite(field) ? field : null;
}

function formatByteCount(value: number): string {
  return `${value.toLocaleString("en-US")} ${value === 1 ? "byte" : "bytes"}`;
}

function terminalCompletionSummary(terminal: Record<string, unknown>, failed: boolean): string {
  const details: string[] = [];
  const failureKind = recordString(terminal, "failureKind");
  if (failed && failureKind) {
    details.push(TERMINAL_FAILURE_LABELS[failureKind] ?? failureKind.replaceAll("_", " "));
  }
  const exitCode = recordNumber(terminal, "exitCode");
  if (exitCode !== null) details.push(`exit ${exitCode}`);
  const signal = recordString(terminal, "signal");
  if (signal) details.push(signal);
  const totalBytes = recordNumber(terminal, "totalBytes");
  if (totalBytes !== null) details.push(formatByteCount(totalBytes));
  if (terminal.truncated === true) details.push("output truncated");

  const label = failed ? "Terminal failed" : "Terminal exited";
  return details.length > 0 ? `${label} (${details.join(", ")})` : label;
}

function terminalLifecycleSummary(event: ControlPlaneSessionEvent): string | null {
  const terminal = terminalData(event);
  if (!terminal) return null;

  switch (recordString(terminal, "eventType")) {
    case "terminal.started": {
      const command = recordString(terminal, "commandSummary");
      const cwd = recordString(terminal, "cwdLabel");
      if (command && cwd) return `Running ${command} in ${cwd}`;
      if (command) return `Running ${command}`;
      return cwd ? `Terminal started in ${cwd}` : "Terminal started";
    }
    case "terminal.output.reference": {
      const details: string[] = [];
      const segmentIndex = recordNumber(terminal, "segmentIndex");
      if (segmentIndex !== null) details.push(`segment ${segmentIndex + 1}`);
      const length = recordNumber(terminal, "length");
      if (length !== null) details.push(formatByteCount(length));
      return details.length > 0
        ? `Terminal output saved to Artifact (${details.join(", ")})`
        : "Terminal output saved to Artifact";
    }
    case "terminal.exited":
      return terminalCompletionSummary(terminal, false);
    case "terminal.failed":
      return terminalCompletionSummary(terminal, true);
    default:
      return null;
  }
}

function hasUnconfirmedWorkspaceCheckpoint(event: ControlPlaneSessionEvent): boolean {
  return ["reason", "risk", "releaseReason"].some((key) =>
    payloadString(event, key)?.includes(WORKSPACE_CHECKPOINT_UNCONFIRMED_RISK),
  );
}

function runtimeItemSummary(event: ControlPlaneSessionEvent): string {
  const terminalSummary = terminalLifecycleSummary(event);
  if (terminalSummary) return terminalSummary;
  const title = payloadString(event, "title");
  const itemType = payloadString(event, "itemType")?.replaceAll("_", " ");
  if (title) return title;
  if (itemType) {
    if (event.eventType === "item.started") return `${itemType} started`;
    if (event.eventType === "item.completed") return `${itemType} completed`;
    return `${itemType} updated`;
  }
  return event.eventType;
}

function eventTurnId(event: ControlPlaneSessionEvent): TurnId | null {
  const value = payloadString(event, "turnId");
  return value ? TurnId.makeUnsafe(value) : null;
}

function eventRuntimeMode(event: ControlPlaneSessionEvent): RuntimeMode | null {
  const value = payloadString(event, "runtimeMode");
  return value === "approval-required" || value === "full-access" ? value : null;
}

function eventInteractionMode(event: ControlPlaneSessionEvent): ProviderInteractionMode | null {
  const value = payloadString(event, "interactionMode");
  return value === "default" || value === "plan" ? value : null;
}

function assistantMessageId(executionId: string): MessageId {
  return MessageId.makeUnsafe(`control-plane-assistant-${executionId}`);
}

function userMessageId(turnId: string): MessageId {
  return MessageId.makeUnsafe(`control-plane-user-${turnId}`);
}

function steerMessageId(event: ControlPlaneSessionEvent): MessageId {
  return MessageId.makeUnsafe(
    `control-plane-steer-${payloadString(event, "controlCommandId") ?? event.eventId}`,
  );
}

function updateAssistantStreaming(
  messages: Thread["messages"],
  executionId: string | null,
  completedAt: string,
): Thread["messages"] {
  if (!executionId) return messages;
  const id = assistantMessageId(executionId);
  let changed = false;
  const next = messages.map((message) => {
    if (message.id !== id || (!message.streaming && message.completedAt === completedAt)) {
      return message;
    }
    changed = true;
    return { ...message, streaming: false, completedAt };
  });
  return changed ? next : messages;
}

function appendAssistantDelta(
  messages: Thread["messages"],
  event: ControlPlaneSessionEvent,
  text: string,
  turnId: TurnId | null,
): Thread["messages"] {
  const executionId = event.executionId ?? `event-${event.eventId}`;
  const id = assistantMessageId(executionId);
  const index = messages.findIndex((message) => message.id === id);
  if (index < 0) {
    return [
      ...messages,
      {
        id,
        role: "assistant",
        text,
        turnId,
        createdAt: event.occurredAt,
        streaming: true,
        source: "native",
      },
    ];
  }
  const current = messages[index]!;
  const next = [...messages];
  next[index] = { ...current, text: `${current.text}${text}`, streaming: true };
  return next;
}

function activitySummary(event: ControlPlaneSessionEvent): string | null {
  if (hasUnconfirmedWorkspaceCheckpoint(event)) {
    return "Workspace checkpoint unconfirmed; local changes may be lost";
  }
  switch (event.eventType) {
    case "execution.leased":
      return "Worker assigned";
    case "execution.started":
      return "Execution started";
    case "execution.recovering":
      return "Execution reconnecting";
    case "execution.completed":
      return "Execution completed";
    case "execution.failed":
      return payloadString(event, "failureMessage") ?? "Execution failed";
    case "execution.cancelled":
      return "Execution cancelled";
    case "turn.interrupt-requested":
      return "Interrupt requested";
    case "turn.steer-requested":
      return "Steer requested";
    case "turn.steered":
      return "Steer applied";
    case "session.compact-requested":
      return "Context compaction requested";
    case "session.compacted":
      return "Context compacted";
    case "session.review-requested":
      return "Review requested";
    case "session.history.rolled-back":
      return "Session history rolled back";
    case "session.forked":
      return payloadString(event, "sourceSessionId") ? "Session fork created" : "Session forked";
    case "execution.interrupted":
      return "Execution interrupted";
    case "approval.requested":
    case "request.opened":
      return "Approval required";
    case "approval.resolved":
    case "request.resolved":
      return "Approval resolved";
    case "user-input.requested":
      return "User input required";
    case "user-input.resolved":
      return "User input resolved";
    case "artifact.ready":
      return "Artifact ready";
    case "workspace.ready":
      return payloadString(event, "restoredCheckpointId")
        ? "Workspace restored"
        : "Workspace ready";
    case "workspace.dirty":
      return "Workspace changed";
    case "workspace.failed":
      return payloadString(event, "failureMessage") ?? "Workspace failed";
    case "checkpoint.created":
      return "Workspace checkpoint started";
    case "checkpoint.ready":
      return "Workspace checkpoint ready";
    case "checkpoint.failed":
      return payloadString(event, "failureMessage") ?? "Workspace checkpoint failed";
    case "session.suspended":
      return "Session suspended";
    case "session.resumed":
      return "Session resumed";
    case "session.archived":
      return "Session archived";
    case "session.model.changed": {
      const model = payloadString(event, "model");
      return model ? `Model switched to ${model}` : "Session model switched";
    }
    case "content.delta":
      return null;
    case "item.started":
    case "item.updated":
    case "item.completed":
      return runtimeItemSummary(event);
    case "thread.token-usage.updated":
      return "Token usage updated";
    case "turn.started":
      return "Provider turn started";
    case "turn.completed":
      return "Provider turn completed";
    case "turn.aborted":
      return payloadString(event, "reason") ?? "Provider turn aborted";
    case "turn.tasks.updated":
      return "Plan tasks updated";
    case "turn.proposed.delta":
    case "turn.proposed.completed":
      return "Plan updated";
    case "turn.diff.updated":
      return "Diff updated";
    case "runtime.warning":
      return payloadString(event, "message") ?? "Provider warning";
    case "runtime.error":
      return payloadString(event, "message") ?? "Provider error";
    default:
      return event.eventType.startsWith("runtime.") || event.eventVersion >= 2
        ? event.eventType
        : null;
  }
}

function appendActivity(
  activities: Thread["activities"],
  event: ControlPlaneSessionEvent,
  turnId: TurnId | null,
): Thread["activities"] {
  const summary = activitySummary(event);
  if (
    !summary ||
    event.eventType === "runtime.output.delta" ||
    event.eventType === "content.delta"
  ) {
    return activities;
  }
  const tone: OrchestrationThreadActivity["tone"] =
    hasUnconfirmedWorkspaceCheckpoint(event) ||
    event.eventType === "execution.failed" ||
    event.eventType === "execution.cancelled" ||
    event.eventType === "workspace.failed" ||
    event.eventType === "checkpoint.failed" ||
    event.eventType === "runtime.error" ||
    event.eventType === "turn.aborted"
      ? "error"
      : event.eventType.includes("approval") ||
          event.eventType.includes("user-input") ||
          event.eventType.startsWith("request.")
        ? "approval"
        : event.eventType.startsWith("runtime.") ||
            event.eventType.startsWith("item.") ||
            event.eventType.startsWith("tool.")
          ? "tool"
          : "info";
  const next = [
    ...activities,
    {
      id: EventId.makeUnsafe(event.eventId),
      tone,
      kind: event.eventType,
      summary,
      payload: {
        ...event.payload,
        ...(event.executionId && event.payload.executionId === undefined
          ? { executionId: event.executionId }
          : {}),
      } as OrchestrationThreadActivity["payload"],
      turnId,
      sequence: event.sequence,
      createdAt: event.occurredAt,
    },
  ];
  return next.length > 500 ? next.slice(next.length - 500) : next;
}

export function createControlPlaneSessionProjection(
  session: ControlPlaneAgentSession,
): ControlPlaneSessionProjection {
  return {
    session,
    lastAppliedSequence: 0,
    durableLastSequence: session.lastEventSequence,
    streamStatus: "idle",
    executionTurnIds: {},
    messages: [],
    activities: [],
    latestTurn: null,
    latestTurnKind: null,
    forkSourceSessionId: null,
    forkSourceEventSequence: null,
    runtimeMode: "full-access",
    interactionMode: "default",
    orchestrationStatus: session.status === "active" ? "ready" : "stopped",
    error: null,
  };
}

export function withControlPlaneStreamStatus(
  projection: ControlPlaneSessionProjection,
  streamStatus: ControlPlaneStreamStatus,
): ControlPlaneSessionProjection {
  return projection.streamStatus === streamStatus ? projection : { ...projection, streamStatus };
}

export function applyControlPlaneSessionEvent(
  projection: ControlPlaneSessionProjection,
  event: ControlPlaneSessionEvent,
): ControlPlaneProjectionApplyResult {
  if (
    event.tenantId !== projection.session.tenantId ||
    event.sessionId !== projection.session.id ||
    event.sequence <= projection.lastAppliedSequence
  ) {
    return { projection, gap: null };
  }
  if (event.sequence !== projection.lastAppliedSequence + 1) {
    return {
      projection,
      gap: {
        afterSequence: projection.lastAppliedSequence,
        receivedSequence: event.sequence,
      },
    };
  }

  let session = projection.session;
  let messages = projection.messages;
  let latestTurn = projection.latestTurn;
  let latestTurnKind = projection.latestTurnKind;
  let forkSourceSessionId = projection.forkSourceSessionId;
  let forkSourceEventSequence = projection.forkSourceEventSequence;
  let runtimeMode = projection.runtimeMode;
  let interactionMode = projection.interactionMode;
  let orchestrationStatus = projection.orchestrationStatus;
  let error = projection.error;
  let executionTurnIds = projection.executionTurnIds;
  let turnId =
    eventTurnId(event) ??
    (event.executionId ? (executionTurnIds[event.executionId] ?? null) : null);

  switch (event.eventType) {
    case "turn.created": {
      const rawTurnId = payloadString(event, "turnId");
      const inputText = payloadString(event, "inputText") ?? "";
      const turnKind = eventTurnKind(event);
      if (rawTurnId) {
        const createdTurnId = TurnId.makeUnsafe(rawTurnId);
        const executionId = event.executionId ?? payloadString(event, "executionId");
        turnId = createdTurnId;
        if (executionId && executionTurnIds[executionId] !== createdTurnId) {
          executionTurnIds = { ...executionTurnIds, [executionId]: createdTurnId };
        }
        if (turnKind === "message") {
          messages = [
            ...messages,
            {
              id: userMessageId(rawTurnId),
              role: "user",
              text: inputText,
              turnId: createdTurnId,
              createdAt: event.occurredAt,
              streaming: false,
              source: "native",
            },
          ];
        }
        latestTurn = {
          turnId: createdTurnId,
          state: "running",
          requestedAt: event.occurredAt,
          startedAt: null,
          completedAt: null,
          assistantMessageId: null,
        };
        latestTurnKind = turnKind;
      }
      runtimeMode = eventRuntimeMode(event) ?? runtimeMode;
      interactionMode = eventInteractionMode(event) ?? interactionMode;
      orchestrationStatus = "starting";
      error = null;
      break;
    }
    case "session.history.rolled-back":
      messages = rollbackMessages(messages, event);
      latestTurn = null;
      latestTurnKind = null;
      orchestrationStatus = "ready";
      error = null;
      break;
    case "session.forked": {
      const sourceSessionId = payloadString(event, "sourceSessionId");
      if (sourceSessionId && sourceSessionId !== projection.session.id) {
        forkSourceSessionId = sourceSessionId;
        forkSourceEventSequence =
          payloadNumber(event, "sourceEventSequence") ??
          payloadNumber(event, "sourceSequence") ??
          forkSourceEventSequence;
      }
      break;
    }
    case "turn.steer-requested": {
      const inputText = payloadString(event, "inputText");
      if (inputText) {
        messages = [
          ...messages,
          {
            id: steerMessageId(event),
            role: "user",
            text: inputText,
            turnId,
            dispatchMode: "steer",
            createdAt: event.occurredAt,
            streaming: false,
            source: "native",
          },
        ];
      }
      orchestrationStatus = "running";
      error = null;
      break;
    }
    case "execution.leased":
      orchestrationStatus = "starting";
      break;
    case "execution.started":
      orchestrationStatus = "running";
      if (latestTurn && (!turnId || latestTurn.turnId === turnId)) {
        latestTurn = {
          ...latestTurn,
          state: "running",
          startedAt: payloadString(event, "startedAt") ?? event.occurredAt,
        };
      }
      break;
    case "execution.recovering":
      orchestrationStatus = "starting";
      break;
    case "runtime.output.delta":
    case "content.delta": {
      const text =
        event.eventType === "content.delta"
          ? payloadString(event, "streamKind") === "assistant_text"
            ? payloadString(event, "delta")
            : null
          : payloadString(event, "text");
      if (text && (latestTurnKind === "message" || latestTurnKind === "review")) {
        messages = appendAssistantDelta(messages, event, text, turnId);
        const executionId = event.executionId ?? `event-${event.eventId}`;
        if (latestTurn && (!turnId || latestTurn.turnId === turnId)) {
          latestTurn = {
            ...latestTurn,
            assistantMessageId: assistantMessageId(executionId),
          };
        }
      }
      orchestrationStatus = "running";
      break;
    }
    case "execution.completed": {
      const completedAt = payloadString(event, "finishedAt") ?? event.occurredAt;
      messages = updateAssistantStreaming(messages, event.executionId, completedAt);
      if (latestTurn && (!turnId || latestTurn.turnId === turnId)) {
        latestTurn = { ...latestTurn, state: "completed", completedAt };
      }
      orchestrationStatus = "ready";
      error = null;
      break;
    }
    case "execution.failed": {
      const completedAt = payloadString(event, "finishedAt") ?? event.occurredAt;
      messages = updateAssistantStreaming(messages, event.executionId, completedAt);
      if (latestTurn && (!turnId || latestTurn.turnId === turnId)) {
        latestTurn = { ...latestTurn, state: "error", completedAt };
      }
      orchestrationStatus = "error";
      error = payloadString(event, "failureMessage") ?? "Execution failed.";
      break;
    }
    case "execution.cancelled": {
      const completedAt = payloadString(event, "finishedAt") ?? event.occurredAt;
      messages = updateAssistantStreaming(messages, event.executionId, completedAt);
      if (latestTurn && (!turnId || latestTurn.turnId === turnId)) {
        latestTurn = { ...latestTurn, state: "interrupted", completedAt };
      }
      orchestrationStatus = "interrupted";
      break;
    }
    case "execution.interrupted": {
      const completedAt = payloadString(event, "finishedAt") ?? event.occurredAt;
      messages = updateAssistantStreaming(messages, event.executionId, completedAt);
      if (latestTurn && (!turnId || latestTurn.turnId === turnId)) {
        latestTurn = { ...latestTurn, state: "interrupted", completedAt };
      }
      orchestrationStatus = "interrupted";
      error = null;
      break;
    }
    case "session.suspended":
      session = { ...session, status: "suspended", updatedAt: event.occurredAt };
      orchestrationStatus = "stopped";
      break;
    case "session.resumed":
      session = { ...session, status: "active", updatedAt: event.occurredAt };
      orchestrationStatus = "ready";
      error = null;
      break;
    case "session.archived":
      session = {
        ...session,
        status: "archived",
        archivedAt: payloadString(event, "archivedAt") ?? event.occurredAt,
        updatedAt: event.occurredAt,
      };
      orchestrationStatus = "stopped";
      break;
    case "session.model.changed": {
      const nextProvider = payloadString(event, "provider");
      session = {
        ...session,
        provider: nextProvider ? providerKind(nextProvider) : session.provider,
        model: payloadString(event, "model") ?? session.model,
        updatedAt: event.occurredAt,
      };
      break;
    }
  }

  return {
    projection: {
      ...projection,
      session: {
        ...session,
        lastEventSequence: Math.max(session.lastEventSequence, event.sequence),
      },
      lastAppliedSequence: event.sequence,
      durableLastSequence: Math.max(projection.durableLastSequence, event.sequence),
      executionTurnIds,
      messages,
      activities: appendActivity(projection.activities, event, turnId),
      latestTurn,
      latestTurnKind,
      forkSourceSessionId,
      forkSourceEventSequence,
      runtimeMode,
      interactionMode,
      orchestrationStatus,
      error,
    },
    gap: null,
  };
}

export function projectControlPlaneProjects(
  projects: ReadonlyArray<ControlPlaneProject>,
  previous: ReadonlyArray<Project> = [],
): Project[] {
  const previousById = new Map(previous.map((project) => [project.id, project] as const));
  return projects.map((project) => {
    const id = ProjectId.makeUnsafe(project.id);
    const existing = previousById.get(id);
    return {
      id,
      kind: "project",
      name: existing?.localName ?? project.name,
      remoteName: project.name,
      folderName: project.name,
      localName: existing?.localName ?? null,
      cwd: `/__synara_control_plane__/${project.tenantId}/${project.id}`,
      defaultModelSelection: null,
      expanded: existing?.expanded ?? true,
      createdAt: project.createdAt,
      updatedAt: project.updatedAt,
      scripts: [],
    };
  });
}

function threadSession(projection: ControlPlaneSessionProjection): ThreadSession {
  const provider = providerKind(projection.session.provider);
  const status: ThreadSession["status"] =
    projection.orchestrationStatus === "running"
      ? "running"
      : projection.orchestrationStatus === "starting"
        ? "connecting"
        : projection.orchestrationStatus === "error"
          ? "error"
          : projection.orchestrationStatus === "stopped"
            ? "closed"
            : "ready";
  return {
    provider,
    status,
    ...(projection.latestTurn?.state === "running"
      ? { activeTurnId: projection.latestTurn.turnId }
      : {}),
    createdAt: projection.session.createdAt,
    updatedAt: projection.session.updatedAt,
    ...(projection.error ? { lastError: projection.error } : {}),
    orchestrationStatus: projection.orchestrationStatus,
  };
}

export function projectControlPlaneThreads(
  sessions: ReadonlyArray<ControlPlaneAgentSession>,
  projections: ReadonlyMap<string, ControlPlaneSessionProjection>,
): Thread[] {
  return sessions.map((session) => {
    const projection = projections.get(session.id) ?? createControlPlaneSessionProjection(session);
    const sourceSession = projection.session;
    const provider = providerKind(sourceSession.provider);
    const model = sourceSession.model ?? getDefaultModel(provider) ?? "default";
    return {
      id: ThreadId.makeUnsafe(sourceSession.id),
      codexThreadId: null,
      projectId: ProjectId.makeUnsafe(sourceSession.projectId),
      title: sourceSession.title,
      modelSelection: { provider, model },
      runtimeMode: projection.runtimeMode,
      interactionMode: projection.interactionMode,
      session: threadSession(projection),
      messages: projection.messages,
      proposedPlans: [],
      error: projection.error,
      createdAt: sourceSession.createdAt,
      archivedAt: sourceSession.archivedAt,
      updatedAt: sourceSession.updatedAt,
      branch: null,
      worktreePath: null,
      envMode: "local",
      latestTurn: projection.latestTurn,
      ...(projection.forkSourceSessionId
        ? { forkSourceThreadId: ThreadId.makeUnsafe(projection.forkSourceSessionId) }
        : {}),
      turnDiffSummaries: [],
      activities: projection.activities,
    };
  });
}
