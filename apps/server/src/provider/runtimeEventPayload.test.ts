import { describe, expect, it } from "vitest";

import {
  makeRuntimeItemLifecyclePayload,
  makeUtf8RuntimeContentDeltaPayload,
} from "./runtimeEventPayload";

describe("runtime event payload normalization", () => {
  it("adds exact UTF-8 command output metadata", () => {
    expect(
      makeUtf8RuntimeContentDeltaPayload({
        streamKind: "command_output",
        terminalId: "terminal-1",
        byteOffset: 8,
        delta: "A🙂",
      }),
    ).toEqual({
      streamKind: "command_output",
      terminalId: "terminal-1",
      encoding: "utf-8",
      byteOffset: 8,
      byteLength: 5,
      delta: "A🙂",
    });
  });

  it("keeps terminal lifecycle data scoped to command items", () => {
    const terminal = {
      terminalId: "terminal-1",
      eventType: "terminal.started" as const,
    };

    expect(
      makeRuntimeItemLifecyclePayload({
        itemType: "command_execution",
        data: { provider: "codex", terminal },
      }),
    ).toEqual({
      itemType: "command_execution",
      data: { provider: "codex", terminal },
    });
    expect(
      makeRuntimeItemLifecyclePayload({
        itemType: "file_change",
        data: { path: "src/app.ts", terminal },
      }),
    ).toEqual({
      itemType: "file_change",
      data: { path: "src/app.ts" },
    });
  });
});
