import { randomUUID } from "node:crypto";

import type { CloudAgentMessageEnvelope } from "@synara/cloud-agent-protocol";
import type {
  ApprovalRequestId,
  ProviderApprovalDecision,
  ProviderRuntimeEvent,
  ProviderSendTurnInput,
  ProviderSession,
  ProviderSessionStartInput,
  ProviderTurnStartResult,
  ProviderUserInputAnswers,
  ThreadId,
  TurnId,
} from "@synara/contracts";
import { Effect, Layer, Queue, Stream } from "effect";

import {
  ProviderAdapterRequestError,
  ProviderAdapterSessionClosedError,
  ProviderAdapterSessionNotFoundError,
  ProviderAdapterValidationError,
  type ProviderAdapterError,
} from "../Errors.ts";
import { CodexAdapter, type CodexAdapterShape } from "../Services/CodexAdapter.ts";
import { PROVIDER_ADAPTER_RUNTIME_EVENT_BUFFER_CAPACITY } from "../Services/ProviderAdapter.ts";
import {
  assertCloudAgentNode24,
  readCloudAgentBackendConfig,
  type CloudAgentBackendConfig,
} from "../cloudAgent/config.ts";
import {
  makeCloudAgentProcessClientFactory,
  resolveDefaultCloudAgentRuntimePath,
  type CloudAgentProcessClient,
  type CloudAgentProcessClientFactory,
} from "../cloudAgent/process.ts";
import { projectCloudAgentMessage } from "../cloudAgent/projection.ts";
import {
  approvalResolution,
  assertCloudAgentCodexDescriptor,
  assertCloudAgentResult,
  assertCloudAgentStopQuiesced,
  cloudAgentRunnerInput,
  decodeCloudAgentCursor,
  encodeCloudAgentCursor,
  makeCloudAgentCommand,
  providerCursorFromTerminal,
  userInputResolution,
} from "../cloudAgent/protocol.ts";

const PROVIDER = "codex" as const;

type PendingInteraction = "approval" | "user-input";

interface CommandContext {
  readonly turnId?: TurnId;
}

interface ActiveCloudAgentSession {
  readonly threadId: ThreadId;
  readonly executionId: string;
  readonly generation: number;
  readonly lifecycleGeneration?: string;
  readonly cwd: string;
  readonly runtimeMode: ProviderSessionStartInput["runtimeMode"];
  readonly model?: string;
  readonly createdAt: string;
  readonly client: CloudAgentProcessClient;
  unsubscribe: () => void;
  readonly commandContexts: Map<string, CommandContext>;
  readonly pendingInteractions: Map<string, PendingInteraction>;
  providerResumeCursor: string | undefined;
  activeCommandId: string | undefined;
  activeTurnId: TurnId | undefined;
  updatedAt: string;
  closed: boolean;
}

export interface CloudAgentCodexAdapterLiveOptions {
  readonly clientFactory?: CloudAgentProcessClientFactory;
  readonly config?: CloudAgentBackendConfig;
  readonly bundledRuntimePath?: string;
}

export function makeCloudAgentCodexAdapterLive(options: CloudAgentCodexAdapterLiveOptions = {}) {
  return Layer.effect(
    CodexAdapter,
    Effect.gen(function* () {
      const config = options.config ?? readCloudAgentBackendConfig();
      if (config.backend !== "cloud-agent") {
        return yield* Effect.die(
          new Error("CloudAgentCodexAdapter cannot be registered unless its backend is selected."),
        );
      }
      assertCloudAgentNode24();
      const clientFactory =
        options.clientFactory ??
        makeCloudAgentProcessClientFactory({
          config,
          ...(config.runtimePath
            ? {}
            : {
                bundledRuntimePath:
                  options.bundledRuntimePath ?? resolveDefaultCloudAgentRuntimePath(),
              }),
        });
      const eventQueue = yield* Queue.bounded<ProviderRuntimeEvent>(
        PROVIDER_ADAPTER_RUNTIME_EVENT_BUFFER_CAPACITY,
      );
      const sessions = new Map<ThreadId, ActiveCloudAgentSession>();
      const fencedThreads = new Set<ThreadId>();
      let generationSequence = 0;

      const sessionSnapshot = (session: ActiveCloudAgentSession): ProviderSession => ({
        provider: PROVIDER,
        status: session.closed ? "closed" : session.activeTurnId ? "running" : "ready",
        runtimeMode: session.runtimeMode,
        cwd: session.cwd,
        ...(session.model ? { model: session.model } : {}),
        threadId: session.threadId,
        ...(encodeCloudAgentCursor(session.providerResumeCursor)
          ? { resumeCursor: encodeCloudAgentCursor(session.providerResumeCursor) }
          : {}),
        ...(session.activeTurnId ? { activeTurnId: session.activeTurnId } : {}),
        createdAt: session.createdAt,
        updatedAt: session.updatedAt,
      });

      const requireSession = (threadId: ThreadId): ActiveCloudAgentSession => {
        const session = sessions.get(threadId);
        if (!session) {
          throw new ProviderAdapterSessionNotFoundError({ provider: PROVIDER, threadId });
        }
        if (session.closed) {
          throw new ProviderAdapterSessionClosedError({ provider: PROVIDER, threadId });
        }
        return session;
      };

      const assertThreadNotFenced = (threadId: ThreadId): void => {
        if (fencedThreads.has(threadId)) {
          throw validation(
            "startSession",
            `thread '${threadId}' is fenced after an unclean Cloud Agent shutdown; restart the server or perform manual recovery before starting it again.`,
          );
        }
      };

      const retireSession = async (
        session: ActiveCloudAgentSession,
        options: { readonly fence: boolean },
      ): Promise<void> => {
        const isCurrent = sessions.get(session.threadId) === session;
        if (options.fence && isCurrent) fencedThreads.add(session.threadId);
        if (isCurrent) sessions.delete(session.threadId);
        if (session.closed) return;
        session.closed = true;
        session.activeCommandId = undefined;
        session.activeTurnId = undefined;
        session.updatedAt = new Date().toISOString();
        session.unsubscribe();
        await session.client.close().catch(() => undefined);
      };

      const executeCloudAgentCommand = async (
        session: ActiveCloudAgentSession,
        command: ReturnType<typeof makeCloudAgentCommand>,
        context: CommandContext = {},
      ) => {
        session.commandContexts.set(command.commandId, context);
        try {
          return await session.client.execute(command);
        } catch (cause) {
          await retireSession(session, { fence: true });
          throw cause;
        } finally {
          session.commandContexts.delete(command.commandId);
        }
      };

      const receiveMessage = async (
        session: ActiveCloudAgentSession,
        message: CloudAgentMessageEnvelope,
      ): Promise<void> => {
        const cursor = providerCursorFromTerminal(message);
        if (cursor) {
          // Cursor persistence must precede the listener acknowledgement that
          // lets the public client resolve the terminal command Promise.
          session.providerResumeCursor = cursor;
          session.updatedAt = new Date().toISOString();
        }
        if (message.messageType === "InteractionRequest") {
          const requestId = message.payload.requestId;
          const interactionType = message.payload.interactionType;
          if (
            typeof requestId === "string" &&
            (interactionType === "approval" || interactionType === "user-input")
          ) {
            session.pendingInteractions.set(requestId, interactionType);
          }
        }
        const context = session.commandContexts.get(message.commandId);
        const event = projectCloudAgentMessage(message, {
          threadId: session.threadId,
          ...(session.lifecycleGeneration
            ? { lifecycleGeneration: session.lifecycleGeneration }
            : {}),
          ...(context?.turnId ? { turnId: context.turnId } : {}),
        });
        if (event) await Effect.runPromise(Queue.offer(eventQueue, event));
      };

      const closeSession = async (threadId: ThreadId): Promise<void> => {
        const session = sessions.get(threadId);
        if (!session) return;
        try {
          const command = makeCloudAgentCommand({
            executionId: session.executionId,
            generation: session.generation,
            commandType: "StopSession",
          });
          const terminal = await executeCloudAgentCommand(session, command);
          assertCloudAgentStopQuiesced(terminal);
        } catch (cause) {
          await retireSession(session, { fence: true });
          throw cause;
        }
        await retireSession(session, { fence: false });
      };

      const startSession = (input: ProviderSessionStartInput) =>
        Effect.tryPromise({
          try: async () => {
            assertThreadNotFenced(input.threadId);
            await closeSession(input.threadId);
            assertThreadNotFenced(input.threadId);
            const cwd = input.cwd ?? process.cwd();
            const client = await clientFactory({ cwd });
            const now = new Date().toISOString();
            const session: ActiveCloudAgentSession = {
              threadId: input.threadId,
              executionId: `synara:${input.threadId}:${randomUUID()}`,
              generation: ++generationSequence,
              ...(input.lifecycleGeneration
                ? { lifecycleGeneration: input.lifecycleGeneration }
                : {}),
              cwd,
              runtimeMode: input.runtimeMode,
              ...(input.modelSelection ? { model: input.modelSelection.model } : {}),
              createdAt: now,
              client,
              unsubscribe: () => {},
              commandContexts: new Map<string, CommandContext>(),
              pendingInteractions: new Map<string, PendingInteraction>(),
              providerResumeCursor: undefined,
              activeCommandId: undefined,
              activeTurnId: undefined,
              updatedAt: now,
              closed: false,
            };
            session.unsubscribe = client.subscribe((message) => receiveMessage(session, message));
            sessions.set(input.threadId, session);
            void client.exit.then(() => {
              if (!session.closed) void retireSession(session, { fence: true });
            });
            try {
              const describe = makeCloudAgentCommand({
                executionId: session.executionId,
                generation: session.generation,
                commandType: "Describe",
                payload: { provider: PROVIDER },
              });
              const described = await executeCloudAgentCommand(session, describe);
              assertCloudAgentResult("Describe", described);
              const descriptor =
                described.messageType === "Result" ? described.payload.descriptor : undefined;
              assertCloudAgentCodexDescriptor(descriptor);

              const cursor = decodeCloudAgentCursor(input.resumeCursor);
              const runnerInput = cloudAgentRunnerInput(input);
              const command = makeCloudAgentCommand({
                executionId: session.executionId,
                generation: session.generation,
                commandType: cursor ? "ResumeSession" : "StartSession",
                payload: {
                  runnerInput: cursor
                    ? { ...runnerInput, providerResumeCursor: cursor.providerResumeCursor }
                    : runnerInput,
                },
              });
              const terminal = await executeCloudAgentCommand(session, command);
              assertCloudAgentResult(command.commandType, terminal);
              session.providerResumeCursor = cursor?.providerResumeCursor;
              session.updatedAt = new Date().toISOString();
              return sessionSnapshot(session);
            } catch (cause) {
              await retireSession(session, { fence: false });
              throw cause;
            }
          },
          catch: (cause) => adapterError("startSession", cause),
        });

      const sendTurn = (input: ProviderSendTurnInput) =>
        Effect.tryPromise({
          try: async (): Promise<ProviderTurnStartResult> => {
            const session = requireSession(input.threadId);
            if (session.activeTurnId) {
              throw validation(
                "sendTurn",
                `thread '${input.threadId}' already has an active turn.`,
              );
            }
            if (input.attachments?.length || input.skills?.length || input.mentions?.length) {
              throw validation(
                "sendTurn",
                "attachments, skill mentions, and agent mentions are unsupported by Cloud Agent Codex.",
              );
            }
            if (input.modelSelection !== undefined) {
              throw validation(
                "sendTurn",
                "per-turn model selection is not representable by Cloud Agent v2.",
              );
            }
            if (input.interactionMode === "plan") {
              throw validation(
                "sendTurn",
                "per-turn plan interaction mode is not representable by Cloud Agent v2.",
              );
            }
            const text = input.input?.trim();
            if (!text) throw validation("sendTurn", "a non-empty text input is required.");
            const turnId = randomUUID() as TurnId;
            const command = makeCloudAgentCommand({
              executionId: session.executionId,
              generation: session.generation,
              commandType: "SendTurn",
              payload: { inputText: text },
            });
            session.activeCommandId = command.commandId;
            session.activeTurnId = turnId;
            session.updatedAt = new Date().toISOString();
            try {
              const terminal = await executeCloudAgentCommand(session, command, { turnId });
              assertCloudAgentResult("SendTurn", terminal);
              return {
                threadId: input.threadId,
                turnId,
                ...(encodeCloudAgentCursor(session.providerResumeCursor)
                  ? { resumeCursor: encodeCloudAgentCursor(session.providerResumeCursor) }
                  : {}),
              };
            } finally {
              session.activeCommandId = undefined;
              session.activeTurnId = undefined;
              session.updatedAt = new Date().toISOString();
            }
          },
          catch: (cause) => adapterError("sendTurn", cause),
        });

      const interruptTurn = (threadId: ThreadId, turnId?: TurnId) =>
        Effect.tryPromise({
          try: async () => {
            const session = requireSession(threadId);
            if (!session.activeCommandId || !session.activeTurnId) {
              throw validation("interruptTurn", `thread '${threadId}' has no active turn.`);
            }
            if (turnId !== undefined && turnId !== session.activeTurnId) {
              throw validation(
                "interruptTurn",
                `turn '${turnId}' is not active for '${threadId}'.`,
              );
            }
            const command = makeCloudAgentCommand({
              executionId: session.executionId,
              generation: session.generation,
              commandType: "InterruptTurn",
              payload: { targetCommandId: session.activeCommandId },
            });
            const terminal = await executeCloudAgentCommand(session, command, {
              turnId: session.activeTurnId,
            });
            assertCloudAgentResult("InterruptTurn", terminal);
          },
          catch: (cause) => adapterError("interruptTurn", cause),
        });

      const resolveInteraction = (
        threadId: ThreadId,
        requestId: ApprovalRequestId,
        type: PendingInteraction,
        resolution: Readonly<Record<string, unknown>>,
      ) =>
        Effect.tryPromise({
          try: async () => {
            const session = requireSession(threadId);
            if (session.pendingInteractions.get(requestId) !== type) {
              throw validation(
                type === "approval" ? "respondToRequest" : "respondToUserInput",
                `request '${requestId}' is not a pending ${type} interaction.`,
              );
            }
            const command = makeCloudAgentCommand({
              executionId: session.executionId,
              generation: session.generation,
              commandType: type === "approval" ? "ResolveApproval" : "ResolveUserInput",
              payload: resolution,
            });
            const terminal = await executeCloudAgentCommand(
              session,
              command,
              session.activeTurnId ? { turnId: session.activeTurnId } : {},
            );
            assertCloudAgentResult(command.commandType, terminal);
            session.pendingInteractions.delete(requestId);
          },
          catch: (cause) =>
            adapterError(type === "approval" ? "respondToRequest" : "respondToUserInput", cause),
        });

      const stopSession = (threadId: ThreadId) =>
        Effect.tryPromise({
          try: () => closeSession(threadId),
          catch: (cause) => adapterError("stopSession", cause),
        });

      const stopAll = () =>
        Effect.forEach([...sessions.keys()], stopSession, {
          concurrency: "unbounded",
          discard: true,
        });

      const adapter: CodexAdapterShape = {
        provider: PROVIDER,
        capabilities: {
          sessionModelSwitch: "unsupported",
          conversationRollback: "restart-session",
          supportsSkillMentions: false,
          supportsSkillDiscovery: false,
          supportsNativeSlashCommandDiscovery: false,
          supportsPluginMentions: false,
          supportsPluginDiscovery: false,
          supportsRuntimeModelList: false,
          supportsTurnSteering: false,
          supportsLiveTurnDiffPatch: true,
        },
        startSession,
        sendTurn,
        interruptTurn,
        respondToRequest: (
          threadId: ThreadId,
          requestId: ApprovalRequestId,
          decision: ProviderApprovalDecision,
        ) =>
          resolveInteraction(
            threadId,
            requestId,
            "approval",
            approvalResolution(requestId, decision),
          ),
        respondToUserInput: (
          threadId: ThreadId,
          requestId: ApprovalRequestId,
          answers: ProviderUserInputAnswers,
        ) =>
          resolveInteraction(
            threadId,
            requestId,
            "user-input",
            userInputResolution(requestId, answers),
          ),
        stopSession,
        listSessions: () => Effect.sync(() => [...sessions.values()].map(sessionSnapshot)),
        hasSession: (threadId) =>
          Effect.sync(() => {
            const session = sessions.get(threadId);
            return session !== undefined && !session.closed;
          }),
        readThread: (threadId) =>
          Effect.try({
            try: () => {
              const session = requireSession(threadId);
              return { threadId, cwd: session.cwd, turns: [] };
            },
            catch: (cause) => adapterError("readThread", cause),
          }),
        rollbackThread: (threadId) =>
          Effect.fail(
            validation(
              "rollbackThread",
              `Cloud Agent Codex cannot roll back thread '${threadId}' without taking history authority.`,
            ),
          ),
        stopAll,
        streamEvents: Stream.fromQueue(eventQueue),
      };
      yield* Effect.acquireRelease(Effect.void, () => stopAll().pipe(Effect.ignore));
      return adapter;
    }),
  );
}

function validation(operation: string, issue: string): ProviderAdapterValidationError {
  return new ProviderAdapterValidationError({ provider: PROVIDER, operation, issue });
}

function adapterError(operation: string, cause: unknown): ProviderAdapterError {
  if (
    cause instanceof ProviderAdapterValidationError ||
    cause instanceof ProviderAdapterRequestError ||
    cause instanceof ProviderAdapterSessionNotFoundError ||
    cause instanceof ProviderAdapterSessionClosedError
  ) {
    return cause;
  }
  return new ProviderAdapterRequestError({
    provider: PROVIDER,
    method: operation,
    detail: cause instanceof Error ? cause.message : String(cause),
    cause,
  });
}
