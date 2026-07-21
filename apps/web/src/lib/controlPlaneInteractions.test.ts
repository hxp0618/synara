import { EventId, type OrchestrationThreadActivity } from "@synara/contracts";
import { describe, expect, it } from "vitest";

import type { ControlPlanePendingInteraction } from "./controlPlaneClient";
import {
  latestControlPlaneInteractionSequence,
  projectPendingControlPlaneInteractions,
} from "./controlPlaneInteractions";

const interaction = (
  input: Partial<ControlPlanePendingInteraction> &
    Pick<ControlPlanePendingInteraction, "id" | "kind" | "requestId" | "payload">,
): ControlPlanePendingInteraction => ({
  executionId: "execution-1",
  turnId: "turn-1",
  provider: "codex",
  requestedAt: "2026-07-13T00:00:00Z",
  expiresAt: "2026-07-14T00:00:00Z",
  ...input,
});

describe("projectPendingControlPlaneInteractions", () => {
  it("projects the durable snapshot into existing approval and user-input view models", () => {
    const projected = projectPendingControlPlaneInteractions([
      interaction({
        id: "interaction-approval",
        kind: "approval",
        requestId: "approval-1",
        payload: { requestType: "exec_command_approval", detail: "bun run build" },
      }),
      interaction({
        id: "interaction-input",
        kind: "user-input",
        requestId: "input-1",
        payload: {
          questions: [
            {
              id: "environment",
              header: "Environment",
              question: "Which environment?",
              options: [{ label: "Staging", description: "Use staging" }],
            },
          ],
        },
      }),
    ]);

    expect(projected.approvals).toEqual([
      expect.objectContaining({
        interactionId: "interaction-approval",
        requestId: "approval-1",
        requestKey: "interaction:interaction-approval",
        lifecycleGeneration: "interaction-approval",
        executionId: "execution-1",
        requestKind: "command",
        detail: "bun run build",
      }),
    ]);
    expect(projected.userInputs).toEqual([
      expect.objectContaining({
        interactionId: "interaction-input",
        requestId: "input-1",
        requestKey: "interaction:interaction-input",
        lifecycleGeneration: "interaction-input",
        executionId: "execution-1",
      }),
    ]);
  });

  it("keeps identical provider request IDs isolated by durable Interaction ID", () => {
    const projected = projectPendingControlPlaneInteractions([
      interaction({
        id: "interaction-1",
        executionId: "execution-1",
        kind: "approval",
        requestId: "shared-request",
        payload: { requestType: "exec_command_approval" },
      }),
      interaction({
        id: "interaction-2",
        executionId: "execution-2",
        kind: "approval",
        requestId: "shared-request",
        payload: { requestType: "exec_command_approval" },
      }),
    ]);

    expect(projected.approvals.map((approval) => approval.requestKey)).toEqual([
      "interaction:interaction-1",
      "interaction:interaction-2",
    ]);
    expect(projected.approvals.map((approval) => approval.lifecycleGeneration)).toEqual([
      "interaction-1",
      "interaction-2",
    ]);
  });
});

describe("latestControlPlaneInteractionSequence", () => {
  it("tracks only events that can change the durable pending snapshot", () => {
    const activity = (
      kind: OrchestrationThreadActivity["kind"],
      sequence: number,
    ): OrchestrationThreadActivity => ({
      id: EventId.makeUnsafe(`${kind}:${sequence}`),
      tone: "info",
      kind,
      summary: kind,
      payload: {},
      turnId: null,
      sequence,
      createdAt: "2026-07-13T00:00:00Z",
    });

    expect(
      latestControlPlaneInteractionSequence([
        activity("runtime.output.delta", 20),
        activity("request.opened", 21),
        activity("runtime.warning", 22),
        activity("execution.recovering", 23),
      ]),
    ).toBe(23);
  });
});
