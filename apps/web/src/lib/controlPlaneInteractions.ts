import { EventId, type OrchestrationThreadActivity } from "@synara/contracts";

import {
  derivePendingApprovals,
  derivePendingUserInputs,
  type PendingApproval,
  type PendingUserInput,
} from "../session-logic";
import type { ControlPlanePendingInteraction } from "./controlPlaneClient";

const INTERACTION_REFRESH_KINDS = new Set<OrchestrationThreadActivity["kind"]>([
  "approval.requested",
  "approval.resolved",
  "request.opened",
  "request.resolved",
  "user-input.requested",
  "user-input.resolved",
  "turn.interrupt-requested",
  "execution.recovering",
  "execution.completed",
  "execution.failed",
  "execution.cancelled",
  "execution.interrupted",
]);

export type ProjectedPendingControlPlaneInteractions = {
  approvals: ReadonlyArray<PendingApproval>;
  userInputs: ReadonlyArray<PendingUserInput>;
};

export function projectPendingControlPlaneInteractions(
  interactions: ReadonlyArray<ControlPlanePendingInteraction>,
): ProjectedPendingControlPlaneInteractions {
  const activities = interactions.map<OrchestrationThreadActivity>((interaction) => ({
    id: EventId.makeUnsafe(`pending-interaction:${interaction.id}`),
    tone: "approval",
    kind: interaction.kind === "approval" ? "request.opened" : "user-input.requested",
    summary: interaction.kind === "approval" ? "Approval required" : "User input required",
    payload: {
      ...interaction.payload,
      interactionId: interaction.id,
      requestId: interaction.requestId,
      executionId: interaction.executionId,
    } as OrchestrationThreadActivity["payload"],
    turnId: null,
    createdAt: interaction.requestedAt,
  }));
  return {
    approvals: derivePendingApprovals(activities),
    userInputs: derivePendingUserInputs(activities),
  };
}

export function latestControlPlaneInteractionSequence(
  activities: ReadonlyArray<OrchestrationThreadActivity>,
): number | null {
  let latest: number | null = null;
  for (const activity of activities) {
    if (!INTERACTION_REFRESH_KINDS.has(activity.kind) || typeof activity.sequence !== "number") {
      continue;
    }
    latest = latest === null ? activity.sequence : Math.max(latest, activity.sequence);
  }
  return latest;
}
