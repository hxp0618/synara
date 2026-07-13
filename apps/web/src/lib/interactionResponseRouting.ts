import type {
  ApprovalRequestId,
  ProviderApprovalDecision,
  ProviderUserInputAnswers,
} from "@synara/contracts";

export async function dispatchApprovalInteractionResponse(input: {
  authoritative: boolean;
  executionId?: string;
  requestId: ApprovalRequestId;
  decision: ProviderApprovalDecision;
  resolveControlPlane: (
    executionId: string,
    requestId: ApprovalRequestId,
    decision: "accept" | "decline",
  ) => Promise<void>;
  interruptControlPlane: () => Promise<void>;
  respondNative: () => Promise<void>;
}): Promise<void> {
  if (!input.authoritative) {
    await input.respondNative();
    return;
  }
  if (input.decision === "acceptForSession") {
    throw new Error("Always allow for this Session is not supported by the SaaS Control Plane.");
  }
  if (input.decision === "cancel") {
    await input.interruptControlPlane();
    return;
  }
  if (!input.executionId) {
    throw new Error("The durable approval is missing its Execution reference.");
  }
  await input.resolveControlPlane(input.executionId, input.requestId, input.decision);
}

export async function dispatchUserInputInteractionResponse(input: {
  authoritative: boolean;
  cancel: boolean;
  executionId?: string;
  requestId: ApprovalRequestId;
  answers: ProviderUserInputAnswers;
  resolveControlPlane: (
    executionId: string,
    requestId: ApprovalRequestId,
    answers: ProviderUserInputAnswers,
  ) => Promise<void>;
  interruptControlPlane: () => Promise<void>;
  respondNative: () => Promise<void>;
}): Promise<void> {
  if (!input.authoritative) {
    await input.respondNative();
    return;
  }
  if (input.cancel) {
    await input.interruptControlPlane();
    return;
  }
  if (!input.executionId) {
    throw new Error("The durable user-input request is missing its Execution reference.");
  }
  await input.resolveControlPlane(input.executionId, input.requestId, input.answers);
}
