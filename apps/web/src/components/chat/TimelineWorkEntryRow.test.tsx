import { describe, expect, it } from "vitest";

import { HammerIcon, WebSearchIcon } from "~/lib/icons";
import type { WorkLogEntry } from "../../session-logic";
import { workEntryLeftIcon } from "./TimelineWorkEntryRow";

function requestEntry(requestKind: "network" | "tool"): WorkLogEntry {
  return {
    id: `${requestKind}-request`,
    createdAt: "2026-07-21T00:00:00.000Z",
    label: "Approval requested",
    tone: "info",
    requestKind,
  };
}

describe("workEntryLeftIcon", () => {
  it("uses semantic icons for network and generic tool approvals", () => {
    expect(workEntryLeftIcon(requestEntry("network"))).toBe(WebSearchIcon);
    expect(workEntryLeftIcon(requestEntry("tool"))).toBe(HammerIcon);
  });
});
