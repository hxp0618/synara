import type {
  CloudAgentCommandEnvelope,
  CloudAgentMessageEnvelope,
} from "@synara/cloud-agent-protocol";
import type { ApprovalRequestId, ThreadId } from "@synara/contracts";
import { Effect, Option, Stream } from "effect";
import { describe, expect, it } from "vitest";

import type { CloudAgentProcessClient } from "../cloudAgent/process.ts";
import { CodexAdapter, type CodexAdapterShape } from "../Services/CodexAdapter.ts";
import { makeCloudAgentCodexAdapterLive } from "./CloudAgentCodexAdapter.ts";

const requiredCapabilities = {
  "start-session": "native",
  "send-turn": "native",
  "interrupt-turn": "native",
  approval: "native",
  "structured-user-input": "native",
};
const threadId = "thread-1" as ThreadId;
const approvalRequestId = "approval-1" as ApprovalRequestId;

class FakeCloudAgentClient implements CloudAgentProcessClient {
  readonly pid = 42;
  readonly exit: Promise<
    | {
        readonly kind: "exit";
        readonly code: number | null;
        readonly signal: NodeJS.Signals | null;
      }
    | { readonly kind: "error"; readonly error: Error }
  >;
  readonly commands: CloudAgentCommandEnvelope[] = [];
  closed = false;
  blockSend = false;
  sendBlocked = false;
  stopOutcome: "quiesced" | "forced" | "timed-out" | "failed" = "quiesced";
  private listener: ((message: CloudAgentMessageEnvelope) => unknown) | undefined;
  private settleSend: (() => void) | undefined;
  private failSend: ((error: Error) => void) | undefined;
  private settleExit!: (termination: {
    readonly kind: "exit";
    readonly code: number | null;
    readonly signal: NodeJS.Signals | null;
  }) => void;
  private exited = false;

  constructor() {
    this.exit = new Promise((resolve) => {
      this.settleExit = resolve;
    });
  }

  subscribe(listener: (message: CloudAgentMessageEnvelope) => unknown): () => void {
    this.listener = listener;
    return () => {
      if (this.listener === listener) this.listener = undefined;
    };
  }

  async execute(command: CloudAgentCommandEnvelope): Promise<CloudAgentMessageEnvelope> {
    this.commands.push(command);
    if (command.commandType === "Describe") {
      return this.emitResult(command, {
        descriptor: {
          providerKind: "codex",
          runtime: { available: true, compatible: true, version: "0.145.0" },
          capabilities: requiredCapabilities,
        },
      });
    }
    if (command.commandType === "SendTurn") {
      await this.emit(command, {
        messageType: "Event",
        payload: {
          eventVersion: 2,
          eventType: "content.delta",
          payload: { streamKind: "assistant_text", delta: "hello" },
        },
      });
      await this.emit(command, {
        messageType: "InteractionRequest",
        payload: {
          interactionType: "approval",
          requestId: "approval-1",
          requestKind: "command",
          command: "git status",
        },
      });
      if (this.blockSend) {
        await new Promise<void>((resolve, reject) => {
          this.settleSend = resolve;
          this.failSend = reject;
          this.sendBlocked = true;
        });
      }
      return this.emitResult(command, { providerResumeCursor: "provider-thread-2" });
    }
    if (command.commandType === "InterruptTurn") {
      const terminal = await this.emitResult(command, {
        interrupted: true,
        providerResumeCursor: "provider-thread-interrupted",
      });
      this.settleSend?.();
      this.settleSend = undefined;
      this.failSend = undefined;
      return terminal;
    }
    if (command.commandType === "StopSession") {
      return this.emitResult(command, {
        stopped: true,
        outcome: this.stopOutcome,
        quiesced: this.stopOutcome === "quiesced",
        graceful: this.stopOutcome === "quiesced",
      });
    }
    return this.emitResult(command, {});
  }

  async close(): Promise<void> {
    this.closed = true;
    this.settleTermination(0, null);
  }

  crash(code = 1, signal: NodeJS.Signals | null = null): void {
    this.failSend?.(new Error(`Cloud Agent Runtime exited (code=${code}).`));
    this.failSend = undefined;
    this.settleSend = undefined;
    this.settleTermination(code, signal);
  }

  private async emitResult(
    command: CloudAgentCommandEnvelope,
    payload: Record<string, unknown>,
  ): Promise<CloudAgentMessageEnvelope> {
    return this.emit(command, { messageType: "Result", payload });
  }

  private async emit(
    command: CloudAgentCommandEnvelope,
    frame:
      | { readonly messageType: "Result"; readonly payload: Record<string, unknown> }
      | { readonly messageType: "Event"; readonly payload: Record<string, unknown> }
      | { readonly messageType: "InteractionRequest"; readonly payload: Record<string, unknown> },
  ): Promise<CloudAgentMessageEnvelope> {
    const message = {
      requestId: command.requestId,
      protocolVersion: command.protocolVersion,
      executionId: command.executionId,
      generation: command.generation,
      commandId: command.commandId,
      occurredAt: new Date().toISOString(),
      ...frame,
    } as CloudAgentMessageEnvelope;
    await this.listener?.(message);
    return message;
  }

  private settleTermination(code: number | null, signal: NodeJS.Signals | null): void {
    if (this.exited) return;
    this.exited = true;
    this.settleExit({ kind: "exit", code, signal });
  }
}

function withAdapterFactory(
  clientFactory: () => Promise<CloudAgentProcessClient>,
  use: (adapter: CodexAdapterShape) => Effect.Effect<void, unknown>,
) {
  const layer = makeCloudAgentCodexAdapterLive({
    config: { backend: "cloud-agent" },
    clientFactory,
  });
  return Effect.scoped(
    Effect.gen(function* () {
      const adapter = yield* CodexAdapter;
      yield* use(adapter);
    }).pipe(Effect.provide(layer)),
  );
}

function withAdapter(
  client: FakeCloudAgentClient,
  use: (adapter: CodexAdapterShape) => Effect.Effect<void, unknown>,
) {
  return withAdapterFactory(async () => client, use);
}

async function waitUntil(predicate: () => Promise<boolean>): Promise<void> {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    if (await predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
  throw new Error("Timed out waiting for Cloud Agent adapter state.");
}

describe("CloudAgentCodexAdapter", () => {
  it("starts, sends, persists the terminal cursor before returning, and keeps local history authority", async () => {
    const client = new FakeCloudAgentClient();
    await Effect.runPromise(
      withAdapter(client, (adapter) =>
        Effect.gen(function* () {
          const started = yield* adapter.startSession({
            threadId,
            provider: "codex",
            runtimeMode: "full-access",
            cwd: process.cwd(),
          });
          expect(started.provider).toBe("codex");
          expect(yield* adapter.hasSession(threadId)).toBe(true);

          const turn = yield* adapter.sendTurn({ threadId, input: "hello" });
          expect(turn.resumeCursor).toMatchObject({
            kind: "synara-cloud-agent-codex",
            providerResumeCursor: "provider-thread-2",
          });
          const event = Option.getOrThrow(yield* Stream.runHead(adapter.streamEvents));
          expect(event).toMatchObject({
            provider: "codex",
            threadId: "thread-1",
            turnId: turn.turnId,
            type: "content.delta",
          });
          expect(yield* adapter.readThread(threadId)).toEqual({
            threadId,
            cwd: process.cwd(),
            turns: [],
          });

          yield* adapter.respondToRequest(threadId, approvalRequestId, "accept");
          expect(client.commands.map((command) => command.commandType)).toContain(
            "ResolveApproval",
          );
          yield* adapter.stopSession(threadId);
          expect(yield* adapter.hasSession(threadId)).toBe(false);
        }),
      ),
    );
    expect(client.closed).toBe(true);
  });

  it("interrupts only the active correlated turn", async () => {
    const client = new FakeCloudAgentClient();
    client.blockSend = true;
    await Effect.runPromise(
      withAdapter(client, (adapter) =>
        Effect.tryPromise({
          try: async () => {
            await Effect.runPromise(
              adapter.startSession({
                threadId,
                provider: "codex",
                runtimeMode: "full-access",
                cwd: process.cwd(),
              }),
            );
            const sending = Effect.runPromise(adapter.sendTurn({ threadId, input: "long task" }));
            while (!client.sendBlocked) {
              await Promise.resolve();
            }
            const active = await Effect.runPromise(adapter.listSessions()).then(
              (sessions) => sessions[0]?.activeTurnId,
            );
            expect(active).toBeDefined();
            await Effect.runPromise(adapter.interruptTurn(threadId, active));
            await sending;
            expect(client.commands.map((command) => command.commandType)).toContain(
              "InterruptTurn",
            );
          },
          catch: (cause) => cause,
        }),
      ),
    );
  });

  it("retires and fences an idle session when its child exits", async () => {
    const client = new FakeCloudAgentClient();
    await Effect.runPromise(
      withAdapter(client, (adapter) =>
        Effect.tryPromise({
          try: async () => {
            await Effect.runPromise(
              adapter.startSession({
                threadId,
                provider: "codex",
                runtimeMode: "full-access",
                cwd: process.cwd(),
              }),
            );
            client.crash(23);
            await waitUntil(() => Effect.runPromise(adapter.hasSession(threadId)).then((v) => !v));
            expect(await Effect.runPromise(adapter.listSessions())).toEqual([]);
            await expect(
              Effect.runPromise(
                adapter.startSession({
                  threadId,
                  provider: "codex",
                  runtimeMode: "full-access",
                  cwd: process.cwd(),
                }),
              ),
            ).rejects.toThrow("is fenced after an unclean Cloud Agent shutdown");
          },
          catch: (cause) => cause,
        }),
      ),
    );
  });

  it("retires and fences an active session on transport failure", async () => {
    const client = new FakeCloudAgentClient();
    client.blockSend = true;
    await Effect.runPromise(
      withAdapter(client, (adapter) =>
        Effect.tryPromise({
          try: async () => {
            await Effect.runPromise(
              adapter.startSession({
                threadId,
                provider: "codex",
                runtimeMode: "full-access",
                cwd: process.cwd(),
              }),
            );
            const sending = Effect.runPromise(
              adapter.sendTurn({ threadId, input: "crash during turn" }),
            );
            await waitUntil(async () => client.sendBlocked);
            client.crash(24);
            await expect(sending).rejects.toThrow("exited (code=24)");
            await waitUntil(() => Effect.runPromise(adapter.hasSession(threadId)).then((v) => !v));
            await expect(
              Effect.runPromise(
                adapter.startSession({
                  threadId,
                  provider: "codex",
                  runtimeMode: "full-access",
                  cwd: process.cwd(),
                }),
              ),
            ).rejects.toThrow("is fenced after an unclean Cloud Agent shutdown");
          },
          catch: (cause) => cause,
        }),
      ),
    );
  });

  it.each(["quiesced", "forced", "timed-out", "failed"] as const)(
    "validates the %s StopSession outcome before allowing replacement",
    async (outcome) => {
      const first = new FakeCloudAgentClient();
      first.stopOutcome = outcome;
      const second = new FakeCloudAgentClient();
      let created = 0;
      await Effect.runPromise(
        withAdapterFactory(
          async () => [first, second][created++]!,
          (adapter) =>
            Effect.tryPromise({
              try: async () => {
                const start = () =>
                  Effect.runPromise(
                    adapter.startSession({
                      threadId,
                      provider: "codex",
                      runtimeMode: "full-access",
                      cwd: process.cwd(),
                    }),
                  );
                await start();
                if (outcome === "quiesced") {
                  await Effect.runPromise(adapter.stopSession(threadId));
                  await start();
                  expect(created).toBe(2);
                  expect(await Effect.runPromise(adapter.hasSession(threadId))).toBe(true);
                } else {
                  await expect(Effect.runPromise(adapter.stopSession(threadId))).rejects.toThrow(
                    `outcome=${outcome}`,
                  );
                  await expect(start()).rejects.toThrow(
                    "is fenced after an unclean Cloud Agent shutdown",
                  );
                  expect(created).toBe(1);
                }
              },
              catch: (cause) => cause,
            }),
        ),
      );
    },
  );

  it("does not spawn a replacement after an unquiesced replacement stop", async () => {
    const first = new FakeCloudAgentClient();
    first.stopOutcome = "forced";
    const second = new FakeCloudAgentClient();
    let created = 0;
    await Effect.runPromise(
      withAdapterFactory(
        async () => [first, second][created++]!,
        (adapter) =>
          Effect.tryPromise({
            try: async () => {
              const start = () =>
                Effect.runPromise(
                  adapter.startSession({
                    threadId,
                    provider: "codex",
                    runtimeMode: "full-access",
                    cwd: process.cwd(),
                  }),
                );
              await start();
              await expect(start()).rejects.toThrow("outcome=forced");
              await expect(start()).rejects.toThrow(
                "is fenced after an unclean Cloud Agent shutdown",
              );
              expect(created).toBe(1);
            },
            catch: (cause) => cause,
          }),
      ),
    );
  });

  it("fails closed on unsupported attachments and foreign cursors", async () => {
    const client = new FakeCloudAgentClient();
    await expect(
      Effect.runPromise(
        withAdapter(client, (adapter) =>
          Effect.gen(function* () {
            yield* adapter.startSession({
              threadId,
              provider: "codex",
              runtimeMode: "full-access",
              cwd: process.cwd(),
              resumeCursor: "native-thread-id",
            });
          }),
        ),
      ),
    ).rejects.toThrow("was not created by CloudAgentCodexAdapter");
  });
});
