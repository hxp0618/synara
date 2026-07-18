// FILE: ComposerPendingUserInputPanel.browser.tsx
// Purpose: Browser regression coverage for durable structured user-input prompts.
// Layer: Chat composer UI browser test

import { ApprovalRequestId } from "@synara/contracts";
import { page } from "vitest/browser";
import { describe, expect, it, vi } from "vitest";
import { render } from "vitest-browser-react";

import type { PendingUserInput } from "../../session-logic";
import type { PendingUserInputDraftAnswer } from "../../pendingUserInput";
import { ComposerPendingUserInputPanel } from "./ComposerPendingUserInputPanel";

const FIRST_REQUEST_ID = ApprovalRequestId.makeUnsafe("user-input-request-1");
const REPLACEMENT_REQUEST_ID = ApprovalRequestId.makeUnsafe("user-input-request-2");

function makePrompt(
  requestId: ApprovalRequestId = FIRST_REQUEST_ID,
  multiSelect = false,
): PendingUserInput {
  return {
    requestId,
    requestKey: `execution-1:${requestId}`,
    executionId: "execution-1",
    createdAt: "2026-07-18T10:00:00.000Z",
    questions: [
      {
        id: "fixture-choice",
        header: "Fixture",
        question: "Choose the deterministic acceptance answer.",
        multiSelect,
        options: [
          { label: "Continue", description: "Continue the deterministic turn." },
          { label: "Stop", description: "Stop the deterministic turn." },
        ],
      },
    ],
  };
}

function renderPanel(input?: {
  prompt?: PendingUserInput;
  onAdvance?: (answers?: Record<string, PendingUserInputDraftAnswer>) => void;
  onToggleOption?: (questionId: string, optionLabel: string) => PendingUserInputDraftAnswer | null;
}) {
  const prompt = input?.prompt ?? makePrompt();
  const onAdvance = vi.fn(input?.onAdvance ?? (() => undefined));
  const onToggleOption = vi.fn(
    input?.onToggleOption ??
      ((_questionId: string, optionLabel: string) => ({
        selectedOptionLabels: [optionLabel],
        customAnswer: "",
      })),
  );
  const element = (activePrompt: PendingUserInput, isResponding = false) => (
    <ComposerPendingUserInputPanel
      pendingUserInputs={[activePrompt]}
      respondingRequestIds={isResponding ? [activePrompt.requestKey ?? activePrompt.requestId] : []}
      answers={{}}
      questionIndex={0}
      onToggleOption={onToggleOption}
      onAdvance={onAdvance}
      onPrevious={() => undefined}
      onCancel={() => undefined}
    />
  );

  return { prompt, onAdvance, onToggleOption, element };
}

describe("ComposerPendingUserInputPanel", () => {
  it("auto-submits a single-select answer with the selected option override", async () => {
    const mounted = renderPanel();
    const screen = await render(mounted.element(mounted.prompt));

    try {
      await page.getByRole("button", { name: /Continue/u }).click();

      await vi.waitFor(() => {
        expect(mounted.onAdvance).toHaveBeenCalledTimes(1);
      });
      expect(mounted.onToggleOption).toHaveBeenCalledWith("fixture-choice", "Continue");
      expect(mounted.onAdvance).toHaveBeenCalledWith({
        "fixture-choice": {
          selectedOptionLabels: ["Continue"],
          customAnswer: "",
        },
      });
    } finally {
      await screen.unmount();
    }
  });

  it("keeps multi-select answers pending until the user advances explicitly", async () => {
    const mounted = renderPanel({ prompt: makePrompt(FIRST_REQUEST_ID, true) });
    const screen = await render(mounted.element(mounted.prompt));

    try {
      await page.getByRole("button", { name: /Continue/u }).click();
      await new Promise((resolve) => window.setTimeout(resolve, 250));

      expect(mounted.onToggleOption).toHaveBeenCalledWith("fixture-choice", "Continue");
      expect(mounted.onAdvance).not.toHaveBeenCalled();
    } finally {
      await screen.unmount();
    }
  });

  it("cancels a stale auto-submit when a replacement request remounts the card", async () => {
    const mounted = renderPanel();
    const screen = await render(mounted.element(mounted.prompt));

    try {
      await page.getByRole("button", { name: /Continue/u }).click();
      await screen.rerender(mounted.element(makePrompt(REPLACEMENT_REQUEST_ID)));
      await new Promise((resolve) => window.setTimeout(resolve, 250));

      expect(mounted.onAdvance).not.toHaveBeenCalled();
      await expect
        .element(page.getByText("Choose the deterministic acceptance answer."))
        .toBeInTheDocument();
    } finally {
      await screen.unmount();
    }
  });

  it("disables answer choices while a response is in flight", async () => {
    const mounted = renderPanel();
    const screen = await render(mounted.element(mounted.prompt, true));

    try {
      await expect.element(page.getByRole("button", { name: /Continue/u })).toBeDisabled();
      await expect.element(page.getByRole("button", { name: /Stop/u })).toBeDisabled();
    } finally {
      await screen.unmount();
    }
  });
});
