// FILE: MessagesTimeline.bootstrapScroll.browser.tsx
// Purpose: Browser regression for initial transcript bottom-stick without LegendList bootstrap ownership.
// Layer: Vitest browser tests

import "../../index.css";

import { MessageId } from "@synara/contracts";
import { afterEach, describe, expect, it, vi } from "vitest";
import { render } from "vitest-browser-react";

import {
  AUTO_SCROLL_BOTTOM_THRESHOLD_PX,
  getScrollContainerDistanceFromBottom,
} from "../../chat-scroll";
import type { deriveTimelineEntries } from "../../session-logic";
import { MessagesTimeline } from "./MessagesTimeline";

type TimelineEntries = ReturnType<typeof deriveTimelineEntries>;

const LEGEND_BOOTSTRAP_WARNING =
  "LegendList bootstrap initial scroll aborted after exceeding convergence bounds.";

function buildTimelineEntries(): TimelineEntries {
  return Array.from({ length: 18 }, (_, index) => {
    const role = index % 2 === 0 ? "user" : "assistant";
    const messageId = MessageId.makeUnsafe(`bootstrap-scroll-message-${index}`);
    return {
      id: `entry-${messageId}`,
      kind: "message" as const,
      createdAt: `2026-07-18T08:${String(index).padStart(2, "0")}:00.000Z`,
      message: {
        id: messageId,
        role,
        text:
          role === "user"
            ? `User message ${index} ${"keeps the transcript tall enough to overflow. ".repeat(4)}`
            : `Assistant message ${index}\n\n${"Detailed response content keeps row measurement active. ".repeat(5)}`,
        createdAt: `2026-07-18T08:${String(index).padStart(2, "0")}:00.000Z`,
        streaming: false,
      },
    };
  });
}

const TIMELINE_ENTRIES = buildTimelineEntries();

function consoleCallContainsText(
  calls: ReadonlyArray<readonly unknown[]>,
  expectedText: string,
): boolean {
  return calls.some((call) =>
    call.some((value) => {
      if (typeof value === "string") {
        return value.includes(expectedText);
      }
      if (value instanceof Error) {
        return value.message.includes(expectedText);
      }
      return false;
    }),
  );
}

async function nextFrame(): Promise<void> {
  await new Promise<void>((resolve) => {
    window.requestAnimationFrame(() => resolve());
  });
}

async function waitForLayout(): Promise<void> {
  await nextFrame();
  await nextFrame();
  await nextFrame();
}

async function waitForScrollContainer(): Promise<HTMLElement> {
  let scrollContainer: HTMLElement | null = null;
  await vi.waitFor(
    () => {
      scrollContainer = document.querySelector<HTMLElement>("[data-chat-scroll-container='true']");
      expect(scrollContainer).not.toBeNull();
    },
    { timeout: 4_000, interval: 16 },
  );
  return scrollContainer!;
}

async function expectNearBottom(scrollContainer: HTMLElement): Promise<void> {
  await vi.waitFor(
    () => {
      expect(getScrollContainerDistanceFromBottom(scrollContainer)).toBeLessThanOrEqual(
        AUTO_SCROLL_BOTTOM_THRESHOLD_PX,
      );
    },
    { timeout: 4_000, interval: 16 },
  );
}

function BootstrapScrollTimeline() {
  return (
    <div style={{ height: 420 }}>
      <MessagesTimeline
        hasMessages
        isWorking={false}
        activeTurnInProgress={false}
        activeTurnStartedAt={null}
        timelineEntries={TIMELINE_ENTRIES}
        turnDiffSummaryByAssistantMessageId={new Map()}
        nowIso="2026-07-18T08:30:00.000Z"
        expandedWorkGroups={{}}
        onToggleWorkGroup={() => {}}
        onOpenTurnDiff={() => {}}
        revertTurnCountByUserMessageId={new Map()}
        onRevertUserMessage={() => {}}
        isRevertingCheckpoint={false}
        onImageExpand={() => {}}
        markdownCwd={undefined}
        resolvedTheme="dark"
        timestampFormat="locale"
        workspaceRoot={undefined}
      />
    </div>
  );
}

describe("MessagesTimeline bootstrap bottom-stick", () => {
  afterEach(() => {
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("avoids LegendList bootstrap warnings on non-empty mount while staying near the bottom", async () => {
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const screen = await render(<BootstrapScrollTimeline />);

    try {
      const scrollContainer = await waitForScrollContainer();
      await waitForLayout();
      await expectNearBottom(scrollContainer);

      expect(consoleCallContainsText(warnSpy.mock.calls, LEGEND_BOOTSTRAP_WARNING)).toBe(false);
      expect(consoleCallContainsText(errorSpy.mock.calls, LEGEND_BOOTSTRAP_WARNING)).toBe(false);
    } finally {
      await screen.unmount();
    }
  });
});
