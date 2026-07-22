import type {
  OrchestrationLatestTurn,
  ProviderInteractionMode,
  ProviderKind,
  RuntimeMode,
} from "@synara/contracts";

import { randomUUID } from "./utils";

export type ControlPlaneSourceProposedPlan = NonNullable<
  OrchestrationLatestTurn["sourceProposedPlan"]
>;

export type ControlPlaneTurnDispatchInput = {
  draftThreadId: string;
  persistedSessionId: string | null;
  projectId: string;
  title: string;
  provider: ProviderKind;
  model?: string;
  inputText: string;
  runtimeMode: RuntimeMode;
  interactionMode: ProviderInteractionMode;
  sourceProposedPlan?: ControlPlaneSourceProposedPlan;
  createSession: (input: {
    projectId: string;
    title: string;
    provider: ProviderKind;
    model?: string;
    idempotencyKey: string;
  }) => Promise<{ id: string }>;
  createTurn: (input: {
    sessionId: string;
    inputText: string;
    runtimeMode: RuntimeMode;
    interactionMode: ProviderInteractionMode;
    sourceProposedPlan?: ControlPlaneSourceProposedPlan;
    idempotencyKey: string;
  }) => Promise<{ id: string }>;
  onSessionResolved?: (input: {
    sessionId: string;
    createdSession: boolean;
  }) => Promise<void> | void;
};

export type ControlPlaneTurnDispatchResult = {
  sessionId: string;
  createdSession: boolean;
  turnId: string;
};

type PendingControlPlaneModelSwitch = {
  sessionId: string;
  targetModel: string;
  idempotencyKey: string;
};

export class ControlPlaneTurnDispatcher {
  readonly #randomUUID: () => string;
  readonly #draftSessionIds = new Map<string, string>();
  readonly #sessionCreateKeys = new Map<string, string>();
  readonly #turnKeys = new Map<string, string>();

  constructor(randomUUID: () => string) {
    this.#randomUUID = randomUUID;
  }

  async dispatch(input: ControlPlaneTurnDispatchInput): Promise<ControlPlaneTurnDispatchResult> {
    let sessionId =
      input.persistedSessionId ?? this.#draftSessionIds.get(input.draftThreadId) ?? null;
    let createdSession = false;
    if (!sessionId) {
      const idempotencyKey =
        this.#sessionCreateKeys.get(input.draftThreadId) ?? `web-session-${this.#randomUUID()}`;
      this.#sessionCreateKeys.set(input.draftThreadId, idempotencyKey);
      const session = await input.createSession({
        projectId: input.projectId,
        title: input.title,
        provider: input.provider,
        ...(input.model ? { model: input.model } : {}),
        idempotencyKey,
      });
      sessionId = session.id;
      createdSession = true;
      this.#draftSessionIds.set(input.draftThreadId, sessionId);
    }

    if (sessionId !== input.persistedSessionId) {
      await input.onSessionResolved?.({ sessionId, createdSession });
    }

    const sourceKey = input.sourceProposedPlan
      ? `${input.sourceProposedPlan.threadId}\u0000${input.sourceProposedPlan.planId}`
      : "";
    const turnRequestKey = `${sessionId}\u0000${input.runtimeMode}\u0000${input.interactionMode}\u0000${sourceKey}\u0000${input.inputText}`;
    const turnIdempotencyKey =
      this.#turnKeys.get(turnRequestKey) ?? `web-turn-${this.#randomUUID()}`;
    this.#turnKeys.set(turnRequestKey, turnIdempotencyKey);
    const turn = await input.createTurn({
      sessionId,
      inputText: input.inputText,
      runtimeMode: input.runtimeMode,
      interactionMode: input.interactionMode,
      ...(input.sourceProposedPlan ? { sourceProposedPlan: input.sourceProposedPlan } : {}),
      idempotencyKey: turnIdempotencyKey,
    });

    this.#turnKeys.delete(turnRequestKey);
    this.#draftSessionIds.delete(input.draftThreadId);
    this.#sessionCreateKeys.delete(input.draftThreadId);
    return { sessionId, createdSession, turnId: turn.id };
  }
}

let sharedControlPlaneTurnDispatcher = new ControlPlaneTurnDispatcher(randomUUID);
let sharedPendingControlPlaneModelSwitches = new Map<string, PendingControlPlaneModelSwitch>();

export function dispatchControlPlaneTurn(
  input: ControlPlaneTurnDispatchInput,
): Promise<ControlPlaneTurnDispatchResult> {
  return sharedControlPlaneTurnDispatcher.dispatch(input);
}

export function reserveSharedControlPlaneModelSwitchIdempotencyKey(input: {
  draftThreadId: string;
  sessionId: string;
  targetModel: string;
}): string {
  const existing = sharedPendingControlPlaneModelSwitches.get(input.draftThreadId);
  if (existing?.sessionId === input.sessionId && existing.targetModel === input.targetModel) {
    return existing.idempotencyKey;
  }
  const idempotencyKey = `web-model-switch-${randomUUID()}`;
  sharedPendingControlPlaneModelSwitches.set(input.draftThreadId, {
    sessionId: input.sessionId,
    targetModel: input.targetModel,
    idempotencyKey,
  });
  return idempotencyKey;
}

export function clearSharedControlPlaneModelSwitchIdempotencyKey(draftThreadId: string): void {
  sharedPendingControlPlaneModelSwitches.delete(draftThreadId);
}

export function resetSharedControlPlaneTurnDispatcherForTests(): void {
  sharedControlPlaneTurnDispatcher = new ControlPlaneTurnDispatcher(randomUUID);
  sharedPendingControlPlaneModelSwitches = new Map();
}
