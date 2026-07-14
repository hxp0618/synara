import type {
  ControlPlaneAgentSession,
  ControlPlaneSessionEvent,
  ControlPlaneSessionEventPage,
} from "./controlPlaneClient";
import {
  applyControlPlaneSessionEvent,
  createControlPlaneSessionProjection,
  withControlPlaneStreamStatus,
  type ControlPlaneSessionProjection,
  type ControlPlaneStreamStatus,
} from "./controlPlaneProjection";

type ProjectionRuntimeClient = {
  listSessionEvents: (
    sessionId: string,
    afterSequence: number,
    limit: number,
  ) => Promise<ControlPlaneSessionEventPage>;
  subscribeSessionEvents: (
    sessionId: string,
    afterSequence: number,
    handlers: {
      onEvent: (event: ControlPlaneSessionEvent) => void;
      onOpen?: () => void;
      onError?: () => void;
    },
  ) => () => void;
};

export type ControlPlaneProjectionRuntimeOptions = {
  client: ProjectionRuntimeClient;
  onChange: (projections: ReadonlyMap<string, ControlPlaneSessionProjection>) => void;
  reconnectDelayMs?: number;
};

type LiveStream = {
  close: () => void;
  reconnectTimer: ReturnType<typeof setTimeout> | null;
};

export class ControlPlaneProjectionRuntime {
  readonly #client: ProjectionRuntimeClient;
  readonly #onChange: ControlPlaneProjectionRuntimeOptions["onChange"];
  readonly #reconnectDelayMs: number;
  #scopeKey = "";
  #generation = 0;
  #sessions = new Map<string, ControlPlaneAgentSession>();
  #projections = new Map<string, ControlPlaneSessionProjection>();
  #watchCounts = new Map<string, number>();
  #catchups = new Map<string, Promise<void>>();
  #streams = new Map<string, LiveStream>();
  #disposed = false;

  constructor(options: ControlPlaneProjectionRuntimeOptions) {
    this.#client = options.client;
    this.#onChange = options.onChange;
    this.#reconnectDelayMs = options.reconnectDelayMs ?? 2_000;
  }

  get projections(): ReadonlyMap<string, ControlPlaneSessionProjection> {
    return this.#projections;
  }

  setScope(scopeKey: string, sessions: ReadonlyArray<ControlPlaneAgentSession>): void {
    if (scopeKey !== this.#scopeKey) {
      this.#generation += 1;
      this.#scopeKey = scopeKey;
      this.#closeAllStreams();
      this.#catchups.clear();
      this.#watchCounts.clear();
      this.#sessions = new Map();
      this.#projections = new Map();
    }

    const nextSessions = new Map(sessions.map((session) => [session.id, session] as const));
    const nextProjections = new Map<string, ControlPlaneSessionProjection>();
    for (const session of sessions) {
      const existing = this.#projections.get(session.id);
      if (!existing) {
        nextProjections.set(session.id, createControlPlaneSessionProjection(session));
        continue;
      }
      nextProjections.set(session.id, {
        ...existing,
        session: {
          ...session,
          lastEventSequence: Math.max(
            session.lastEventSequence,
            existing.session.lastEventSequence,
          ),
        },
        durableLastSequence: Math.max(existing.durableLastSequence, session.lastEventSequence),
      });
    }
    for (const sessionId of this.#streams.keys()) {
      if (!nextSessions.has(sessionId)) this.#closeStream(sessionId);
    }
    this.#sessions = nextSessions;
    this.#projections = nextProjections;
    this.#notify();
  }

  async catchUpAll(): Promise<void> {
    await Promise.all([...this.#sessions.keys()].map((sessionId) => this.catchUp(sessionId)));
  }

  catchUp(sessionId: string): Promise<void> {
    const existing = this.#catchups.get(sessionId);
    if (existing) return existing;
    const generation = this.#generation;
    const task = this.#runCatchUp(sessionId, generation).finally(() => {
      if (this.#catchups.get(sessionId) === task) this.#catchups.delete(sessionId);
    });
    this.#catchups.set(sessionId, task);
    return task;
  }

  watch(sessionId: string): () => void {
    const count = this.#watchCounts.get(sessionId) ?? 0;
    this.#watchCounts.set(sessionId, count + 1);
    if (count === 0) void this.#ensureLive(sessionId);
    let active = true;
    return () => {
      if (!active) return;
      active = false;
      const nextCount = Math.max(0, (this.#watchCounts.get(sessionId) ?? 1) - 1);
      if (nextCount === 0) {
        this.#watchCounts.delete(sessionId);
        this.#closeStream(sessionId);
        this.#setStreamStatus(sessionId, "idle");
      } else {
        this.#watchCounts.set(sessionId, nextCount);
      }
    };
  }

  dispose(): void {
    this.#disposed = true;
    this.#generation += 1;
    this.#closeAllStreams();
    this.#catchups.clear();
    this.#watchCounts.clear();
  }

  async #runCatchUp(sessionId: string, generation: number): Promise<void> {
    if (!this.#sessions.has(sessionId) || this.#disposed) return;
    this.#setStreamStatus(sessionId, "catching-up");
    try {
      while (generation === this.#generation && !this.#disposed) {
        const projection = this.#projections.get(sessionId);
        if (!projection) return;
        const page = await this.#client.listSessionEvents(
          sessionId,
          projection.lastAppliedSequence,
          500,
        );
        if (generation !== this.#generation || this.#disposed) return;
        let nextProjection = this.#projections.get(sessionId);
        if (!nextProjection) return;
        for (const event of page.items) {
          const applied = applyControlPlaneSessionEvent(nextProjection, event);
          if (applied.gap) {
            throw new Error(
              `Session Event gap after ${applied.gap.afterSequence}; received ${applied.gap.receivedSequence}.`,
            );
          }
          nextProjection = applied.projection;
        }
        nextProjection = {
          ...nextProjection,
          durableLastSequence: Math.max(nextProjection.durableLastSequence, page.lastSequence),
        };
        this.#projections.set(sessionId, nextProjection);
        this.#notify();
        if (nextProjection.lastAppliedSequence >= page.lastSequence) break;
        if (page.items.length === 0) {
          throw new Error(
            `Session Event backlog stopped at ${nextProjection.lastAppliedSequence} of ${page.lastSequence}.`,
          );
        }
      }
      if (generation === this.#generation && !this.#disposed) {
        this.#setStreamStatus(
          sessionId,
          (this.#watchCounts.get(sessionId) ?? 0) > 0 ? "connecting" : "idle",
        );
      }
    } catch {
      if (generation !== this.#generation || this.#disposed) return;
      this.#setStreamStatus(sessionId, "error");
      if ((this.#watchCounts.get(sessionId) ?? 0) > 0) this.#scheduleReconnect(sessionId);
      throw new Error(`Failed to catch up Session Events for ${sessionId}.`);
    }
  }

  async #ensureLive(sessionId: string): Promise<void> {
    if (
      this.#disposed ||
      !this.#sessions.has(sessionId) ||
      (this.#watchCounts.get(sessionId) ?? 0) === 0 ||
      this.#streams.has(sessionId)
    ) {
      return;
    }
    const generation = this.#generation;
    try {
      await this.catchUp(sessionId);
    } catch {
      return;
    }
    if (
      generation !== this.#generation ||
      this.#disposed ||
      (this.#watchCounts.get(sessionId) ?? 0) === 0 ||
      this.#streams.has(sessionId)
    ) {
      return;
    }
    const projection = this.#projections.get(sessionId);
    if (!projection) return;
    this.#setStreamStatus(sessionId, "connecting");
    try {
      const close = this.#client.subscribeSessionEvents(sessionId, projection.lastAppliedSequence, {
        onOpen: () => {
          if (generation === this.#generation) this.#setStreamStatus(sessionId, "live");
        },
        onError: () => {
          if (generation !== this.#generation) return;
          this.#closeStream(sessionId);
          this.#setStreamStatus(sessionId, "reconnecting");
          this.#scheduleReconnect(sessionId);
        },
        onEvent: (event) => {
          if (generation !== this.#generation) return;
          const current = this.#projections.get(sessionId);
          if (!current) return;
          const applied = applyControlPlaneSessionEvent(current, event);
          if (applied.gap) {
            this.#closeStream(sessionId);
            this.#setStreamStatus(sessionId, "reconnecting");
            void this.catchUp(sessionId).then(
              () => this.#ensureLive(sessionId),
              () => undefined,
            );
            return;
          }
          if (applied.projection !== current) {
            this.#projections.set(sessionId, applied.projection);
            this.#notify();
          }
          this.#setStreamStatus(sessionId, "live");
        },
      });
      this.#streams.set(sessionId, { close, reconnectTimer: null });
    } catch {
      this.#setStreamStatus(sessionId, "reconnecting");
      this.#scheduleReconnect(sessionId);
    }
  }

  #setStreamStatus(sessionId: string, status: ControlPlaneStreamStatus): void {
    const projection = this.#projections.get(sessionId);
    if (!projection) return;
    const next = withControlPlaneStreamStatus(projection, status);
    if (next === projection) return;
    this.#projections.set(sessionId, next);
    this.#notify();
  }

  #scheduleReconnect(sessionId: string): void {
    if (
      this.#disposed ||
      (this.#watchCounts.get(sessionId) ?? 0) === 0 ||
      !this.#sessions.has(sessionId)
    ) {
      return;
    }
    const existing = this.#streams.get(sessionId);
    if (existing?.reconnectTimer) return;
    const generation = this.#generation;
    const timer = setTimeout(() => {
      const stream = this.#streams.get(sessionId);
      if (stream?.reconnectTimer === timer) this.#streams.delete(sessionId);
      if (generation === this.#generation) void this.#ensureLive(sessionId);
    }, this.#reconnectDelayMs);
    this.#streams.set(sessionId, { close: () => undefined, reconnectTimer: timer });
  }

  #closeStream(sessionId: string): void {
    const stream = this.#streams.get(sessionId);
    if (!stream) return;
    this.#streams.delete(sessionId);
    if (stream.reconnectTimer) clearTimeout(stream.reconnectTimer);
    stream.close();
  }

  #closeAllStreams(): void {
    for (const sessionId of [...this.#streams.keys()]) this.#closeStream(sessionId);
  }

  #notify(): void {
    this.#onChange(new Map(this.#projections));
  }
}
