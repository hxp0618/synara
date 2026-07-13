import { ApprovalRequestId } from "@synara/contracts";
import { describe, expect, it, vi } from "vitest";

import {
  dispatchApprovalInteractionResponse,
  dispatchUserInputInteractionResponse,
} from "./interactionResponseRouting";

const requestId = ApprovalRequestId.makeUnsafe("request-1");

describe("Interaction response routing", () => {
  it("never calls Native approval resolution in authoritative SaaS mode", async () => {
    const resolveControlPlane = vi.fn(async () => undefined);
    const interruptControlPlane = vi.fn(async () => undefined);
    const respondNative = vi.fn(async () => undefined);

    await dispatchApprovalInteractionResponse({
      authoritative: true,
      executionId: "execution-1",
      requestId,
      decision: "accept",
      resolveControlPlane,
      interruptControlPlane,
      respondNative,
    });

    expect(resolveControlPlane).toHaveBeenCalledWith("execution-1", requestId, "accept");
    expect(respondNative).not.toHaveBeenCalled();
    expect(interruptControlPlane).not.toHaveBeenCalled();
  });

  it("routes SaaS cancel through Interrupt and rejects unsupported Session-wide approval", async () => {
    const resolveControlPlane = vi.fn(async () => undefined);
    const interruptControlPlane = vi.fn(async () => undefined);
    const respondNative = vi.fn(async () => undefined);

    await dispatchApprovalInteractionResponse({
      authoritative: true,
      executionId: "execution-1",
      requestId,
      decision: "cancel",
      resolveControlPlane,
      interruptControlPlane,
      respondNative,
    });
    expect(interruptControlPlane).toHaveBeenCalledOnce();
    await expect(
      dispatchApprovalInteractionResponse({
        authoritative: true,
        executionId: "execution-1",
        requestId,
        decision: "acceptForSession",
        resolveControlPlane,
        interruptControlPlane,
        respondNative,
      }),
    ).rejects.toThrow("not supported");
    expect(respondNative).not.toHaveBeenCalled();
  });

  it("preserves the Native approval and user-input paths in local mode", async () => {
    const approvalNative = vi.fn(async () => undefined);
    const inputNative = vi.fn(async () => undefined);
    const resolveApproval = vi.fn(async () => undefined);
    const resolveInput = vi.fn(async () => undefined);
    const interruptControlPlane = vi.fn(async () => undefined);

    await dispatchApprovalInteractionResponse({
      authoritative: false,
      requestId,
      decision: "acceptForSession",
      resolveControlPlane: resolveApproval,
      interruptControlPlane,
      respondNative: approvalNative,
    });
    await dispatchUserInputInteractionResponse({
      authoritative: false,
      cancel: true,
      requestId,
      answers: {},
      resolveControlPlane: resolveInput,
      interruptControlPlane,
      respondNative: inputNative,
    });

    expect(approvalNative).toHaveBeenCalledOnce();
    expect(inputNative).toHaveBeenCalledOnce();
    expect(resolveApproval).not.toHaveBeenCalled();
    expect(resolveInput).not.toHaveBeenCalled();
    expect(interruptControlPlane).not.toHaveBeenCalled();
  });

  it("routes SaaS structured input and cancel without touching Native", async () => {
    const resolveControlPlane = vi.fn(async () => undefined);
    const interruptControlPlane = vi.fn(async () => undefined);
    const respondNative = vi.fn(async () => undefined);

    await dispatchUserInputInteractionResponse({
      authoritative: true,
      cancel: false,
      executionId: "execution-1",
      requestId,
      answers: { environment: "staging" },
      resolveControlPlane,
      interruptControlPlane,
      respondNative,
    });
    await dispatchUserInputInteractionResponse({
      authoritative: true,
      cancel: true,
      executionId: "execution-1",
      requestId,
      answers: {},
      resolveControlPlane,
      interruptControlPlane,
      respondNative,
    });

    expect(resolveControlPlane).toHaveBeenCalledWith("execution-1", requestId, {
      environment: "staging",
    });
    expect(interruptControlPlane).toHaveBeenCalledOnce();
    expect(respondNative).not.toHaveBeenCalled();
  });
});
