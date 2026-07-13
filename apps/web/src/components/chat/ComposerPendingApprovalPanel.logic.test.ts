import { describe, expect, it } from "vitest";

import { availableApprovalActions } from "./ComposerPendingApprovalPanel";

describe("availableApprovalActions", () => {
  it("hides unsupported Session-wide approval in SaaS mode", () => {
    expect(availableApprovalActions(false).map((action) => action.decision)).toEqual([
      "accept",
      "decline",
      "cancel",
    ]);
    expect(availableApprovalActions(true).map((action) => action.decision)).toContain(
      "acceptForSession",
    );
  });
});
