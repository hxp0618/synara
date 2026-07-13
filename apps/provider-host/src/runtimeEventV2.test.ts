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
