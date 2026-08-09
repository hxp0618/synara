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
  readonly commands: CloudAgentCommandEnvelope[] = [];
  closed = false;
  blockSend = false;
  sendBlocked = false;
  private listener: ((message: CloudAgentMessageEnvelope) => unknown) | undefined;
  private settleSend: (() => void) | undefined;

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
        await new Promise<void>((resolve) => {
          this.settleSend = resolve;
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
      return terminal;
    }
    return this.emitResult(command, {});
  }

  async close(): Promise<void> {
    this.closed = true;
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
}

function withAdapter(
  client: FakeCloudAgentClient,
  use: (adapter: CodexAdapterShape) => Effect.Effect<void, unknown>,
) {
  const layer = makeCloudAgentCodexAdapterLive({
    config: { backend: "cloud-agent" },
    clientFactory: async () => client,
  });
  return Effect.scoped(
    Effect.gen(function* () {
      const adapter = yield* CodexAdapter;
      yield* use(adapter);
    }).pipe(Effect.provide(layer)),
  );
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
