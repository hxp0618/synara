import {
  ThreadId,
  type ProviderInstanceId,
  type ProviderKind,
  type ProviderSession,
} from "@t3tools/contracts";
import { it, assert, vi } from "@effect/vitest";
import { assertFailure } from "@effect/vitest/utils";

import { Effect, Layer, Stream } from "effect";

import { ClaudeAdapter, ClaudeAdapterShape } from "../Services/ClaudeAdapter.ts";
import { CodexAdapter, CodexAdapterShape } from "../Services/CodexAdapter.ts";
import { CursorAdapter, CursorAdapterShape } from "../Services/CursorAdapter.ts";
import { GeminiAdapter, GeminiAdapterShape } from "../Services/GeminiAdapter.ts";
import { GrokAdapter, GrokAdapterShape } from "../Services/GrokAdapter.ts";
import { KiloAdapter, KiloAdapterShape } from "../Services/KiloAdapter.ts";
import { OpenCodeAdapter, OpenCodeAdapterShape } from "../Services/OpenCodeAdapter.ts";
import { PiAdapter, PiAdapterShape } from "../Services/PiAdapter.ts";
import { ProviderAdapterRegistry } from "../Services/ProviderAdapterRegistry.ts";
import { ProviderAdapterRegistryLive } from "./ProviderAdapterRegistry.ts";
import { ProviderUnsupportedError } from "../Errors.ts";
import * as NodeServices from "@effect/platform-node/NodeServices";
import { ServerSettingsService } from "../../serverSettings.ts";

const asProviderInstanceId = (value: string): ProviderInstanceId => value as ProviderInstanceId;

const fakeCodexAdapter: CodexAdapterShape = {
  provider: "codex",
  capabilities: { sessionModelSwitch: "in-session" },
  startSession: vi.fn(),
  sendTurn: vi.fn(),
  interruptTurn: vi.fn(),
  respondToRequest: vi.fn(),
  respondToUserInput: vi.fn(),
  stopSession: vi.fn(),
  listSessions: vi.fn(),
  hasSession: vi.fn(),
  readThread: vi.fn(),
  rollbackThread: vi.fn(),
  stopAll: vi.fn(),
  streamEvents: Stream.empty,
};

const fakeClaudeAdapter: ClaudeAdapterShape = {
  provider: "claudeAgent",
  capabilities: { sessionModelSwitch: "in-session" },
  startSession: vi.fn(),
  sendTurn: vi.fn(),
  interruptTurn: vi.fn(),
  respondToRequest: vi.fn(),
  respondToUserInput: vi.fn(),
  stopSession: vi.fn(),
  listSessions: vi.fn(),
  hasSession: vi.fn(),
  readThread: vi.fn(),
  rollbackThread: vi.fn(),
  stopAll: vi.fn(),
  streamEvents: Stream.empty,
};

const fakeCursorAdapter: CursorAdapterShape = {
  provider: "cursor",
  capabilities: { sessionModelSwitch: "in-session" },
  startSession: vi.fn(),
  sendTurn: vi.fn(),
  interruptTurn: vi.fn(),
  respondToRequest: vi.fn(),
  respondToUserInput: vi.fn(),
  stopSession: vi.fn(),
  listSessions: vi.fn(),
  hasSession: vi.fn(),
  readThread: vi.fn(),
  rollbackThread: vi.fn(),
  stopAll: vi.fn(),
  streamEvents: Stream.empty,
};

const fakeGeminiAdapter: GeminiAdapterShape = {
  provider: "gemini",
  capabilities: { sessionModelSwitch: "in-session" },
  startSession: vi.fn(),
  sendTurn: vi.fn(),
  interruptTurn: vi.fn(),
  respondToRequest: vi.fn(),
  respondToUserInput: vi.fn(),
  stopSession: vi.fn(),
  listSessions: vi.fn(),
  hasSession: vi.fn(),
  readThread: vi.fn(),
  rollbackThread: vi.fn(),
  stopAll: vi.fn(),
  streamEvents: Stream.empty,
};

const fakeGrokAdapter: GrokAdapterShape = {
  provider: "grok",
  capabilities: { sessionModelSwitch: "restart-session" },
  startSession: vi.fn(),
  sendTurn: vi.fn(),
  interruptTurn: vi.fn(),
  respondToRequest: vi.fn(),
  respondToUserInput: vi.fn(),
  stopSession: vi.fn(),
  listSessions: vi.fn(),
  hasSession: vi.fn(),
  readThread: vi.fn(),
  rollbackThread: vi.fn(),
  stopAll: vi.fn(),
  streamEvents: Stream.empty,
};

const fakeOpenCodeAdapter: OpenCodeAdapterShape = {
  provider: "opencode",
  capabilities: { sessionModelSwitch: "in-session" },
  startSession: vi.fn(),
  sendTurn: vi.fn(),
  interruptTurn: vi.fn(),
  respondToRequest: vi.fn(),
  respondToUserInput: vi.fn(),
  stopSession: vi.fn(),
  listSessions: vi.fn(),
  hasSession: vi.fn(),
  readThread: vi.fn(),
  rollbackThread: vi.fn(),
  stopAll: vi.fn(),
  streamEvents: Stream.empty,
};

const fakeKiloAdapter: KiloAdapterShape = {
  provider: "kilo",
  capabilities: { sessionModelSwitch: "in-session" },
  startSession: vi.fn(),
  sendTurn: vi.fn(),
  interruptTurn: vi.fn(),
  respondToRequest: vi.fn(),
  respondToUserInput: vi.fn(),
  stopSession: vi.fn(),
  listSessions: vi.fn(),
  hasSession: vi.fn(),
  readThread: vi.fn(),
  rollbackThread: vi.fn(),
  stopAll: vi.fn(),
  streamEvents: Stream.empty,
};

const fakePiAdapter: PiAdapterShape = {
  provider: "pi",
  capabilities: { sessionModelSwitch: "in-session" },
  startSession: vi.fn(),
  sendTurn: vi.fn(),
  interruptTurn: vi.fn(),
  respondToRequest: vi.fn(),
  respondToUserInput: vi.fn(),
  stopSession: vi.fn(),
  listSessions: vi.fn(),
  hasSession: vi.fn(),
  readThread: vi.fn(),
  rollbackThread: vi.fn(),
  stopAll: vi.fn(),
  streamEvents: Stream.empty,
};

const layer = it.layer(
  Layer.mergeAll(
    Layer.provide(
      ProviderAdapterRegistryLive,
      Layer.mergeAll(
        Layer.succeed(CodexAdapter, fakeCodexAdapter),
        Layer.succeed(ClaudeAdapter, fakeClaudeAdapter),
        Layer.succeed(CursorAdapter, fakeCursorAdapter),
        Layer.succeed(GeminiAdapter, fakeGeminiAdapter),
        Layer.succeed(GrokAdapter, fakeGrokAdapter),
        Layer.succeed(KiloAdapter, fakeKiloAdapter),
        Layer.succeed(OpenCodeAdapter, fakeOpenCodeAdapter),
        Layer.succeed(PiAdapter, fakePiAdapter),
        ServerSettingsService.layerTest({
          providerInstances: {
            codex_work: {
              driver: "codex",
              displayName: "Codex Work",
              config: {
                homePath: "/tmp/codex-work",
              },
            },
          },
        }),
      ),
    ),
    NodeServices.layer,
  ),
);

layer("ProviderAdapterRegistryLive", (it) => {
  it.effect("resolves a registered provider adapter", () =>
    Effect.gen(function* () {
      const registry = yield* ProviderAdapterRegistry;
      const codex = yield* registry.getByProvider("codex");
      const claude = yield* registry.getByProvider("claudeAgent");
      const cursor = yield* registry.getByProvider("cursor");
      const gemini = yield* registry.getByProvider("gemini");
      const grok = yield* registry.getByProvider("grok");
      const kilo = yield* registry.getByProvider("kilo");
      const opencode = yield* registry.getByProvider("opencode");
      const pi = yield* registry.getByProvider("pi");
      assert.equal(codex, fakeCodexAdapter);
      assert.equal(claude, fakeClaudeAdapter);
      assert.equal(cursor, fakeCursorAdapter);
      assert.equal(gemini, fakeGeminiAdapter);
      assert.equal(grok, fakeGrokAdapter);
      assert.equal(kilo, fakeKiloAdapter);
      assert.equal(opencode, fakeOpenCodeAdapter);
      assert.equal(pi, fakePiAdapter);

      const providers = yield* registry.listProviders();
      assert.deepEqual(providers, [
        "codex",
        "claudeAgent",
        "cursor",
        "gemini",
        "grok",
        "kilo",
        "opencode",
        "pi",
      ]);
    }),
  );

  it.effect("fails with ProviderUnsupportedError for unknown providers", () =>
    Effect.gen(function* () {
      const registry = yield* ProviderAdapterRegistry;
      const adapter = yield* registry.getByProvider("unknown" as ProviderKind).pipe(Effect.result);
      assertFailure(adapter, new ProviderUnsupportedError({ provider: "unknown" }));
    }),
  );

  it.effect("resolves a settings-backed provider instance facade", () =>
    Effect.gen(function* () {
      const registry = yield* ProviderAdapterRegistry;
      assert.ok(registry.getByInstance);
      assert.ok(registry.listInstances);
      const instanceAdapter = yield* registry.getByInstance(asProviderInstanceId("codex_work"));

      assert.equal(instanceAdapter.provider, "codex");
      assert.notEqual(instanceAdapter, fakeCodexAdapter);

      const instances = yield* registry.listInstances();
      assert.ok(instances.includes(asProviderInstanceId("codex_work")));
    }),
  );

  it.effect("stamps and filters sessions through the provider instance facade", () =>
    Effect.gen(function* () {
      const workInstanceId = asProviderInstanceId("codex_work");
      const defaultThreadId = ThreadId.makeUnsafe("thread-default");
      const workThreadId = ThreadId.makeUnsafe("thread-work");
      const now = new Date().toISOString();
      const sessions: ProviderSession[] = [
        {
          provider: "codex",
          status: "ready",
          runtimeMode: "full-access",
          threadId: defaultThreadId,
          createdAt: now,
          updatedAt: now,
        },
        {
          provider: "codex",
          providerInstanceId: workInstanceId,
          status: "ready",
          runtimeMode: "full-access",
          threadId: workThreadId,
          createdAt: now,
          updatedAt: now,
        },
      ];
      vi.mocked(fakeCodexAdapter.listSessions).mockReturnValue(Effect.succeed(sessions));

      const registry = yield* ProviderAdapterRegistry;
      assert.ok(registry.getByInstance);
      const instanceAdapter = yield* registry.getByInstance(workInstanceId);
      const instanceSessions = yield* instanceAdapter.listSessions();

      assert.deepEqual(
        instanceSessions.map((session) => session.threadId),
        [workThreadId],
      );
      assert.equal(instanceSessions[0]?.providerInstanceId, workInstanceId);
    }),
  );
});
