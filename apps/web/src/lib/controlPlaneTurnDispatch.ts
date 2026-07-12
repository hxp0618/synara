export type ControlPlaneTurnDispatchInput = {
  draftThreadId: string;
  persistedSessionId: string | null;
  projectId: string;
  title: string;
  provider: string;
  model?: string;
  inputText: string;
  createSession: (input: {
    projectId: string;
    title: string;
    provider: string;
    model?: string;
    idempotencyKey: string;
  }) => Promise<{ id: string }>;
  createTurn: (input: {
    sessionId: string;
    inputText: string;
    idempotencyKey: string;
  }) => Promise<unknown>;
};

export type ControlPlaneTurnDispatchResult = {
  sessionId: string;
  createdSession: boolean;
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
        this.#sessionCreateKeys.get(input.draftThreadId) ??
        `web-session-${this.#randomUUID()}`;
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

    const turnRequestKey = `${sessionId}\u0000${input.inputText}`;
    const turnIdempotencyKey =
      this.#turnKeys.get(turnRequestKey) ?? `web-turn-${this.#randomUUID()}`;
    this.#turnKeys.set(turnRequestKey, turnIdempotencyKey);
    await input.createTurn({
      sessionId,
      inputText: input.inputText,
      idempotencyKey: turnIdempotencyKey,
    });

    this.#turnKeys.delete(turnRequestKey);
    this.#draftSessionIds.delete(input.draftThreadId);
    this.#sessionCreateKeys.delete(input.draftThreadId);
    return { sessionId, createdSession };
  }
}
