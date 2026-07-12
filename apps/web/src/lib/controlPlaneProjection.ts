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
  messages: Thread["messages"];
  activities: Thread["activities"];
  latestTurn: OrchestrationLatestTurn | null;
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
  "gemini",
  "grok",
  "kilo",
  "opencode",
  "pi",
]);

function providerKind(value: string): ProviderKind {
  if (value === "claude") return "claudeAgent";
  return PROVIDERS.has(value as ProviderKind) ? (value as ProviderKind) : "codex";
}

function payloadString(event: ControlPlaneSessionEvent, key: string): string | null {
  const value = event.payload[key];
  return typeof value === "string" && value.length > 0 ? value : null;
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
        turnId: eventTurnId(event),
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
    case "execution.interrupted":
      return "Execution interrupted";
    case "approval.requested":
      return "Approval required";
    case "approval.resolved":
      return "Approval resolved";
    case "user-input.requested":
      return "User input required";
    case "user-input.resolved":
      return "User input resolved";
    case "artifact.ready":
      return "Artifact ready";
    case "session.suspended":
      return "Session suspended";
    case "session.resumed":
      return "Session resumed";
    case "session.archived":
      return "Session archived";
    default:
      return event.eventType.startsWith("runtime.") ? event.eventType : null;
  }
}

function appendActivity(
  activities: Thread["activities"],
  event: ControlPlaneSessionEvent,
): Thread["activities"] {
  const summary = activitySummary(event);
  if (!summary || event.eventType === "runtime.output.delta") return activities;
  const tone: OrchestrationThreadActivity["tone"] =
    event.eventType === "execution.failed" || event.eventType === "execution.cancelled"
      ? "error"
      : event.eventType.includes("approval") || event.eventType.includes("user-input")
        ? "approval"
        : event.eventType.startsWith("runtime.")
          ? "tool"
          : "info";
  const next = [
    ...activities,
    {
      id: EventId.makeUnsafe(event.eventId),
      tone,
      kind: event.eventType,
      summary,
      payload: event.payload as OrchestrationThreadActivity["payload"],
      turnId: eventTurnId(event),
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
    messages: [],
    activities: [],
    latestTurn: null,
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
  let runtimeMode = projection.runtimeMode;
  let interactionMode = projection.interactionMode;
  let orchestrationStatus = projection.orchestrationStatus;
  let error = projection.error;
  const turnId = eventTurnId(event);

  switch (event.eventType) {
    case "turn.created": {
      const rawTurnId = payloadString(event, "turnId");
      const inputText = payloadString(event, "inputText") ?? "";
      if (rawTurnId) {
        messages = [
          ...messages,
          {
            id: userMessageId(rawTurnId),
            role: "user",
            text: inputText,
            turnId: TurnId.makeUnsafe(rawTurnId),
            createdAt: event.occurredAt,
            streaming: false,
            source: "native",
          },
        ];
        latestTurn = {
          turnId: TurnId.makeUnsafe(rawTurnId),
          state: "running",
          requestedAt: event.occurredAt,
          startedAt: null,
          completedAt: null,
          assistantMessageId: null,
        };
      }
      runtimeMode = eventRuntimeMode(event) ?? runtimeMode;
      interactionMode = eventInteractionMode(event) ?? interactionMode;
      orchestrationStatus = "starting";
      error = null;
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
    case "runtime.output.delta": {
      const text = payloadString(event, "text");
      if (text) {
        messages = appendAssistantDelta(messages, event, text);
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
      messages,
      activities: appendActivity(projection.activities, event),
      latestTurn,
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
    const provider = providerKind(session.provider);
    const model = session.model ?? getDefaultModel(provider) ?? "default";
    return {
      id: ThreadId.makeUnsafe(session.id),
      codexThreadId: null,
      projectId: ProjectId.makeUnsafe(session.projectId),
      title: session.title,
      modelSelection: { provider, model },
      runtimeMode: projection.runtimeMode,
      interactionMode: projection.interactionMode,
      session: threadSession(projection),
      messages: projection.messages,
      proposedPlans: [],
      error: projection.error,
      createdAt: session.createdAt,
      archivedAt: projection.session.archivedAt,
      updatedAt: projection.session.updatedAt,
      branch: null,
      worktreePath: null,
      envMode: "local",
      latestTurn: projection.latestTurn,
      turnDiffSummaries: [],
      activities: projection.activities,
    };
  });
}
