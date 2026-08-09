import { PROVIDER_RUNTIME_EVENT_VERSION } from "@synara/contracts";
import { describe, expect, it } from "vitest";

import { normalizeRuntimeEventV2 } from "./runtimeEventV2";

describe("Runtime Event v2 normalization", () => {
  it("maps legacy assistant output onto canonical content.delta", () => {
    expect(
      normalizeRuntimeEventV2({
        type: "event",
        eventType: "runtime.output.delta",
        payload: { text: "hello" },
      }),
    ).toEqual({
      eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
      eventType: "content.delta",
      payload: { streamKind: "assistant_text", delta: "hello" },
    });
  });

  it("maps tool lifecycle and keeps only bounded provider references", () => {
    expect(
      normalizeRuntimeEventV2({
        type: "event",
        eventType: "runtime.provider.activity",
        payload: {
          provider: "claudeAgent",
          itemType: "Bash",
          itemId: "tool-1",
          status: "failed",
          secret: "must-not-cross-the-wire",
        },
      }),
    ).toEqual({
      eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
      eventType: "item.completed",
      payload: {
        itemType: "command_execution",
        status: "failed",
        title: "Bash",
        data: {
          provider: "claudeAgent",
          sourceItemType: "Bash",
          providerItemId: "tool-1",
        },
      },
    });
  });

  it("projects safe terminal lifecycle metadata and command output correlation", () => {
    expect(
      normalizeRuntimeEventV2({
        type: "event",
        eventType: "runtime.provider.activity",
        payload: {
          provider: "codex",
          itemType: "commandExecution",
          itemId: "command-1",
          status: "failed",
          terminalId: "command-1",
          terminalEventType: "terminal.failed",
          commandSummary: "bun run test",
          cwdLabel: "apps/provider-host",
          exitCode: 1,
          failureKind: "exit",
          secret: "must-not-cross-the-wire",
        },
      }),
    ).toEqual({
      eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
      eventType: "item.completed",
      payload: {
        itemType: "command_execution",
        status: "failed",
        title: "commandExecution",
        data: {
          provider: "codex",
          sourceItemType: "commandExecution",
          providerItemId: "command-1",
          terminal: {
            terminalId: "command-1",
            eventType: "terminal.failed",
            commandSummary: "bun run test",
            cwdLabel: "apps/provider-host",
            exitCode: 1,
            failureKind: "exit",
            totalBytes: 0,
            previewBytes: 0,
            segmentCount: 0,
            truncated: false,
          },
        },
      },
    });

    expect(
      normalizeRuntimeEventV2({
        type: "event",
        eventType: "runtime.command.output",
        payload: {
          provider: "codex",
          terminalId: "command-1",
          encoding: "utf-8",
          text: "tests passed\n",
          byteOffset: 0,
          byteLength: 13,
        },
      }),
    ).toEqual({
      eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
      eventType: "content.delta",
      payload: {
        streamKind: "command_output",
        delta: "tests passed\n",
        terminalId: "command-1",
        encoding: "utf-8",
        byteOffset: 0,
        byteLength: 13,
      },
    });
  });

  it("normalizes Provider-specific usage fields", () => {
    expect(
      normalizeRuntimeEventV2({
        type: "event",
        eventType: "runtime.usage",
        payload: {
          provider: "claudeAgent",
          input_tokens: 4,
          cache_read_input_tokens: 3,
          output_tokens: 2,
        },
      }),
    ).toEqual({
      eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
      eventType: "thread.token-usage.updated",
      payload: {
        usage: {
          usedTokens: 9,
          lastUsedTokens: 9,
          inputTokens: 4,
          lastInputTokens: 4,
          cachedInputTokens: 3,
          lastCachedInputTokens: 3,
          outputTokens: 2,
          lastOutputTokens: 2,
        },
      },
    });
  });

  it.each([
    ["enteredReviewMode", "review_entered"],
    ["exitedReviewMode", "review_exited"],
    ["contextCompaction", "context_compaction"],
  ] as const)("maps %s to the dedicated canonical item type", (sourceItemType, itemType) => {
    expect(
      normalizeRuntimeEventV2({
        type: "event",
        eventType: "runtime.provider.activity",
        payload: {
          provider: "claudeAgent",
          itemType: sourceItemType,
          itemId: `item-${sourceItemType}`,
          status: "completed",
          supportMode: "emulated",
          reviewTarget: { type: "baseBranch", branch: "main" },
        },
      }),
    ).toEqual({
      eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
      eventType: "item.completed",
      payload: {
        itemType,
        status: "completed",
        title: sourceItemType,
        data: {
          provider: "claudeAgent",
          supportMode: "emulated",
          ...(itemType === "review_entered" || itemType === "review_exited"
            ? { reviewTarget: { type: "baseBranch", branch: "main" } }
            : {}),
          sourceItemType,
          providerItemId: `item-${sourceItemType}`,
        },
      },
    });
  });

  it("projects only stable resume-fallback outcome fields into warning detail", () => {
    expect(
      normalizeRuntimeEventV2({
        type: "event",
        eventType: "runtime.provider.warning",
        payload: {
          provider: "codex",
          message:
            "Native Codex resume failed before turn activity; authoritative-history fallback selected.",
          kind: "session_resume",
          attemptedStrategy: "native-cursor",
          selectedStrategy: "authoritative-history",
          outcome: "fallback_selected",
          reasonCode: "session_resume_invalid",
          fallbackSafety: "before_turn_activity",
          authoritativeHistorySequence: 31,
          providerResumeCursor: "cursor-must-not-cross-the-wire",
          rawError: "error-must-not-cross-the-wire",
          secret: "secret-must-not-cross-the-wire",
        },
      }),
    ).toEqual({
      eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
      eventType: "runtime.warning",
      payload: {
        message:
          "Native Codex resume failed before turn activity; authoritative-history fallback selected.",
        detail: {
          provider: "codex",
          kind: "session_resume",
          attemptedStrategy: "native-cursor",
          selectedStrategy: "authoritative-history",
          outcome: "fallback_selected",
          reasonCode: "session_resume_invalid",
          fallbackSafety: "before_turn_activity",
          authoritativeHistorySequence: 31,
        },
      },
    });
  });

  it("passes canonical events through and degrades unknown internal events safely", () => {
    expect(
      normalizeRuntimeEventV2({
        type: "event",
        eventType: "runtime.warning",
        payload: { message: "already canonical" },
      }),
    ).toEqual({
      eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
      eventType: "runtime.warning",
      payload: { message: "already canonical" },
    });

    expect(
      normalizeRuntimeEventV2({
        type: "event",
        eventType: "provider.future.native-event",
        payload: { token: "must-not-cross-the-wire" },
      }),
    ).toEqual({
      eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
      eventType: "runtime.warning",
      payload: {
        message: "Provider Host ignored an unsupported internal runtime event.",
        detail: { sourceEventType: "provider.future.native-event" },
      },
    });
  });
});
