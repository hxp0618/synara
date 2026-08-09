import type { CloudAgentMessageEnvelope } from "@synara/cloud-agent-protocol";
import type { ThreadId, TurnId } from "@synara/contracts";
import { describe, expect, it } from "vitest";

import { projectCloudAgentMessage } from "./projection.ts";

const base = {
  requestId: "request-1",
  protocolVersion: { major: 2, minor: 3 },
  executionId: "execution-1",
  generation: 1,
  commandId: "command-1",
  occurredAt: "2026-08-09T00:00:00.000Z",
};
const threadId = "thread-1" as ThreadId;
const turnId = "turn-1" as TurnId;

describe("projectCloudAgentMessage", () => {
  it("projects Runtime Event v2 without changing ProviderKind", () => {
    const event = projectCloudAgentMessage(
      {
        ...base,
        messageType: "Event",
        payload: {
          eventVersion: 2,
          eventType: "content.delta",
          payload: { streamKind: "assistant_text", delta: "hello" },
        },
      } as CloudAgentMessageEnvelope,
      { threadId, turnId, lifecycleGeneration: "generation-1" },
    );
    expect(event).toMatchObject({
      provider: "codex",
      threadId: "thread-1",
      turnId: "turn-1",
      type: "content.delta",
      payload: { streamKind: "assistant_text", delta: "hello" },
    });
  });

  it("projects typed approval and user-input interactions", () => {
    const approval = projectCloudAgentMessage(
      {
        ...base,
        messageType: "InteractionRequest",
        payload: {
          interactionType: "approval",
          requestId: "approval-1",
          requestKind: "command",
          command: "git status",
        },
      } as CloudAgentMessageEnvelope,
      { threadId, turnId },
    );
    expect(approval).toMatchObject({
      type: "request.opened",
      requestId: "approval-1",
      payload: { requestType: "command_execution_approval", detail: "git status" },
    });

    const userInput = projectCloudAgentMessage(
      {
        ...base,
        messageType: "InteractionRequest",
        payload: {
          interactionType: "user-input",
          requestId: "question-1",
          questions: [
            {
              id: "environment",
              header: "Environment",
              question: "Where?",
              options: [{ label: "Staging", description: "Use staging" }],
            },
          ],
        },
      } as CloudAgentMessageEnvelope,
      { threadId, turnId },
    );
    expect(userInput).toMatchObject({
      type: "user-input.requested",
      requestId: "question-1",
    });
  });

  it("does not take Artifact or Checkpoint authority", () => {
    for (const messageType of ["ArtifactCandidate", "Checkpoint"] as const) {
      expect(
        projectCloudAgentMessage(
          { ...base, messageType, payload: { candidate: "ignored" } } as CloudAgentMessageEnvelope,
          { threadId },
        ),
      ).toBeUndefined();
    }
  });
});
