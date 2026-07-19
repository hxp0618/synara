import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";
import { Schema } from "effect";

import {
  PROVIDER_RUNTIME_EVENT_TYPES,
  PROVIDER_RUNTIME_EVENT_VERSION,
  ProviderRuntimeEvent,
} from "./providerRuntime";
import type { ProviderRuntimeEventType } from "./providerRuntime";

const decodeRuntimeEvent = Schema.decodeUnknownSync(ProviderRuntimeEvent);

describe("ProviderRuntimeEvent", () => {
  const eventBase = {
    eventId: "event-terminal-1",
    provider: "codex",
    createdAt: "2026-02-28T00:00:00.000Z",
    threadId: "thread-terminal-1",
    turnId: "turn-terminal-1",
  } as const;

  it("keeps the persisted Runtime Event v2 JSON schema aligned with the canonical vocabulary", () => {
    const schema = JSON.parse(
      readFileSync(
        new URL("../../../docs/contracts/runtime-event-v2.schema.json", import.meta.url),
        "utf8",
      ),
    ) as {
      properties: {
        eventVersion: { const: number };
        eventType: { enum: ReadonlyArray<string> };
      };
    };

    expect(schema.properties.eventVersion.const).toBe(PROVIDER_RUNTIME_EVENT_VERSION);
    expect(schema.properties.eventType.enum).toEqual(PROVIDER_RUNTIME_EVENT_TYPES);
  });

  it("includes turn.steered in the exported event type", () => {
    const eventType: ProviderRuntimeEventType = "turn.steered";
    expect(eventType).toBe("turn.steered");
  });

  it("decodes turn.tasks.updated for task-list rendering", () => {
    const parsed = decodeRuntimeEvent({
      type: "turn.tasks.updated",
      eventId: "event-1",
      provider: "claudeAgent",
      sessionId: "runtime-session-1",
      createdAt: "2026-02-28T00:00:00.000Z",
      threadId: "thread-1",
      turnId: "turn-1",
      payload: {
        explanation: "Implement schema updates",
        tasks: [
          { task: "Define event union", status: "completed" },
          { task: "Wire adapter mapping", status: "inProgress" },
        ],
      },
    });

    expect(parsed.type).toBe("turn.tasks.updated");
    if (parsed.type !== "turn.tasks.updated") {
      throw new Error("expected turn.tasks.updated");
    }
    expect(parsed.payload.tasks).toHaveLength(2);
    expect(parsed.payload.tasks[1]?.status).toBe("inProgress");
  });

  it("decodes proposed-plan completion events", () => {
    const parsed = decodeRuntimeEvent({
      type: "turn.proposed.completed",
      eventId: "event-proposed-plan-1",
      provider: "codex",
      createdAt: "2026-02-28T00:00:00.000Z",
      threadId: "thread-1",
      turnId: "turn-1",
      payload: {
        planMarkdown: "# Ship it",
      },
    });

    expect(parsed.type).toBe("turn.proposed.completed");
    if (parsed.type !== "turn.proposed.completed") {
      throw new Error("expected turn.proposed.completed");
    }
    expect(parsed.payload.planMarkdown).toBe("# Ship it");
  });

  it("decodes a large Turn Diff Artifact reference", () => {
    const parsed = decodeRuntimeEvent({
      type: "turn.diff.updated",
      ...eventBase,
      payload: {
        artifact: {
          artifactId: "artifact-diff-1",
          contentType: "text/x-diff; charset=utf-8",
          sizeBytes: 131_072,
          sha256: "a".repeat(64),
          fileCount: 2,
          additions: 120,
          deletions: 40,
        },
      },
    });

    expect(parsed.type).toBe("turn.diff.updated");
    if (
      parsed.type !== "turn.diff.updated" ||
      !("artifact" in parsed.payload) ||
      parsed.payload.artifact === undefined
    ) {
      throw new Error("expected an Artifact-backed turn.diff.updated");
    }
    expect(parsed.payload.artifact.fileCount).toBe(2);
  });

  it("rejects mixed or non-canonical Turn Diff Artifact payloads", () => {
    const artifact = {
      artifactId: "artifact-diff-1",
      contentType: "text/x-diff; charset=utf-8",
      sizeBytes: 131_072,
      sha256: "a".repeat(64),
      fileCount: 2,
      additions: 120,
      deletions: 40,
    } as const;

    expect(() =>
      decodeRuntimeEvent({
        type: "turn.diff.updated",
        ...eventBase,
        payload: { unifiedDiff: "patch", artifact },
      }),
    ).toThrow();
    expect(() =>
      decodeRuntimeEvent({
        type: "turn.diff.updated",
        ...eventBase,
        payload: { artifact: { ...artifact, contentType: "text/plain" } },
      }),
    ).toThrow();
  });

  it("decodes user-input.requested with structured questions", () => {
    const parsed = decodeRuntimeEvent({
      type: "user-input.requested",
      eventId: "event-2",
      provider: "claudeAgent",
      sessionId: "runtime-session-2",
      createdAt: "2026-02-28T00:00:01.000Z",
      threadId: "thread-2",
      requestId: "request-1",
      payload: {
        questions: [
          {
            id: "sandbox_mode",
            header: "Sandbox",
            question: "Which mode should be used?",
            options: [
              {
                label: "workspace-write",
                description: "Allow edits in workspace only",
              },
              {
                label: "danger-full-access",
                description: "Allow unrestricted access",
              },
            ],
          },
        ],
      },
    });

    expect(parsed.type).toBe("user-input.requested");
    if (parsed.type !== "user-input.requested") {
      throw new Error("expected user-input.requested");
    }
    expect(parsed.payload.questions[0]?.id).toBe("sandbox_mode");
    expect(parsed.payload.questions[0]?.options).toHaveLength(2);
  });

  it("decodes user-input.resolved with answer map", () => {
    const parsed = decodeRuntimeEvent({
      type: "user-input.resolved",
      eventId: "event-3",
      provider: "claudeAgent",
      sessionId: "runtime-session-2",
      createdAt: "2026-02-28T00:00:02.000Z",
      threadId: "thread-2",
      requestId: "request-1",
      payload: {
        answers: {
          sandbox_mode: "workspace-write",
        },
      },
    });

    expect(parsed.type).toBe("user-input.resolved");
    if (parsed.type !== "user-input.resolved") {
      throw new Error("expected user-input.resolved");
    }
    expect(parsed.payload.answers.sandbox_mode).toBe("workspace-write");
  });

  it("rejects legacy message.delta type", () => {
    expect(() =>
      decodeRuntimeEvent({
        type: "message.delta",
        eventId: "event-4",
        provider: "codex",
        sessionId: "runtime-session-3",
        createdAt: "2026-02-28T00:00:03.000Z",
        payload: { delta: "legacy" },
      }),
    ).toThrow();
  });

  it("rejects empty branded canonical ids", () => {
    expect(() =>
      decodeRuntimeEvent({
        type: "runtime.error",
        eventId: "event-5",
        provider: "codex",
        sessionId: "runtime-session-3",
        createdAt: "2026-02-28T00:00:03.000Z",
        threadId: "   ",
        payload: { message: "boom" },
      }),
    ).toThrow();
  });

  it("decodes normalized thread token usage snapshots", () => {
    const parsed = decodeRuntimeEvent({
      type: "thread.token-usage.updated",
      eventId: "event-token-usage-1",
      provider: "claudeAgent",
      createdAt: "2026-02-28T00:00:04.000Z",
      threadId: "thread-1",
      payload: {
        usage: {
          usedTokens: 31251,
          usedPercent: 15.6255,
          maxTokens: 200000,
          toolUses: 25,
          durationMs: 43567,
        },
      },
    });

    expect(parsed.type).toBe("thread.token-usage.updated");
    if (parsed.type !== "thread.token-usage.updated") {
      throw new Error("expected thread.token-usage.updated");
    }
    expect(parsed.payload.usage.maxTokens).toBe(200000);
    expect(parsed.payload.usage.usedTokens).toBe(31251);
    expect(parsed.payload.usage.usedPercent).toBe(15.6255);
  });

  it("requires complete command-output byte metadata and exact UTF-8 lengths", () => {
    const valid = {
      ...eventBase,
      type: "content.delta",
      payload: {
        streamKind: "command_output",
        terminalId: "terminal-1",
        encoding: "utf-8",
        byteOffset: 0,
        byteLength: 5,
        delta: "A🙂",
      },
    };

    expect(decodeRuntimeEvent(valid).type).toBe("content.delta");

    for (const field of ["terminalId", "encoding", "byteOffset", "byteLength"] as const) {
      const payload = { ...valid.payload } as Record<string, unknown>;
      delete payload[field];
      expect(() => decodeRuntimeEvent({ ...valid, payload })).toThrow();
    }

    expect(() =>
      decodeRuntimeEvent({
        ...valid,
        payload: { ...valid.payload, byteLength: 4 },
      }),
    ).toThrow();
    expect(() =>
      decodeRuntimeEvent({
        ...valid,
        payload: { ...valid.payload, delta: "\ud800", byteLength: 3 },
      }),
    ).toThrow();
  });

  it("accepts only canonical base64 command output with the decoded byte length", () => {
    const valid = {
      ...eventBase,
      type: "content.delta",
      payload: {
        streamKind: "command_output",
        terminalId: "terminal-1",
        encoding: "binary",
        byteOffset: 8,
        byteLength: 4,
        delta: "AAEC/w==",
      },
    };

    expect(decodeRuntimeEvent(valid).type).toBe("content.delta");
    expect(() =>
      decodeRuntimeEvent({ ...valid, payload: { ...valid.payload, delta: "AB==", byteLength: 1 } }),
    ).toThrow();
    expect(() =>
      decodeRuntimeEvent({ ...valid, payload: { ...valid.payload, delta: "AAEC/w==\n" } }),
    ).toThrow();
    expect(() =>
      decodeRuntimeEvent({ ...valid, payload: { ...valid.payload, byteLength: 3 } }),
    ).toThrow();
  });

  it("keeps non-command content deltas backward compatible", () => {
    const parsed = decodeRuntimeEvent({
      ...eventBase,
      type: "content.delta",
      payload: {
        streamKind: "assistant_text",
        delta: "hello",
      },
    });

    expect(parsed.type).toBe("content.delta");
  });

  it("decodes typed terminal lifecycle data and rejects invalid references", () => {
    const started = decodeRuntimeEvent({
      ...eventBase,
      type: "item.started",
      itemId: "item-terminal-1",
      payload: {
        itemType: "command_execution",
        status: "inProgress",
        data: {
          provider: "codex",
          terminal: {
            terminalId: "terminal-1",
            eventType: "terminal.started",
            commandSummary: "bun run test",
            cwdLabel: "apps/provider-host",
          },
        },
      },
    });
    expect(started.type).toBe("item.started");
    if (started.type !== "item.started" || started.payload.itemType !== "command_execution") {
      throw new Error("expected command execution item");
    }
    const startedData = started.payload.data;
    if (
      !startedData ||
      typeof startedData !== "object" ||
      Array.isArray(startedData) ||
      !("terminal" in startedData)
    ) {
      throw new Error("expected typed command data");
    }
    expect(startedData.terminal?.eventType).toBe("terminal.started");

    const reference = {
      ...eventBase,
      type: "item.updated",
      payload: {
        itemType: "command_execution",
        status: "inProgress",
        data: {
          terminal: {
            terminalId: "terminal-1",
            eventType: "terminal.output.reference",
            artifactId: "550e8400-e29b-41d4-a716-446655440000",
            offset: 0,
            length: 1_048_576,
            segmentIndex: 0,
            encoding: "binary",
          },
        },
      },
    };
    expect(decodeRuntimeEvent(reference).type).toBe("item.updated");

    for (const [field, value] of [
      ["artifactId", "not-a-uuid"],
      ["offset", -1],
      ["length", -1],
      ["segmentIndex", -1],
    ] as const) {
      expect(() =>
        decodeRuntimeEvent({
          ...reference,
          payload: {
            ...reference.payload,
            data: {
              terminal: { ...reference.payload.data.terminal, [field]: value },
            },
          },
        }),
      ).toThrow();
    }
  });

  it("validates terminal completion counters and failure metadata", () => {
    const completed = {
      ...eventBase,
      type: "item.completed",
      payload: {
        itemType: "command_execution",
        status: "failed",
        data: {
          terminal: {
            terminalId: "terminal-1",
            eventType: "terminal.failed",
            totalBytes: 65_536,
            previewBytes: 32_768,
            segmentCount: 1,
            truncated: true,
            exitCode: 137,
            signal: "SIGKILL",
            failureKind: "oom",
          },
        },
      },
    };

    expect(decodeRuntimeEvent(completed).type).toBe("item.completed");
    expect(() =>
      decodeRuntimeEvent({
        ...completed,
        payload: {
          ...completed.payload,
          data: {
            terminal: { ...completed.payload.data.terminal, previewBytes: 65_537 },
          },
        },
      }),
    ).toThrow();
    expect(() =>
      decodeRuntimeEvent({
        ...completed,
        payload: {
          ...completed.payload,
          data: {
            terminal: { ...completed.payload.data.terminal, segmentCount: -1 },
          },
        },
      }),
    ).toThrow();
  });

  it("rejects terminal lifecycle data on non-command items", () => {
    expect(() =>
      decodeRuntimeEvent({
        ...eventBase,
        type: "item.started",
        payload: {
          itemType: "mcp_tool_call",
          data: {
            terminal: {
              terminalId: "terminal-1",
              eventType: "terminal.started",
            },
          },
        },
      }),
    ).toThrow();
  });
});
