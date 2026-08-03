import { describe, expect, it } from "vitest";

import {
  formatUsageBytes,
  formatUsageCost,
  formatUsageDuration,
  formatUsagePeriod,
  formatUsageTokens,
  groupSessionUsageByTurn,
  usageProgressPercent,
} from "./usageDisplay";

describe("Control Plane usage display", () => {
  it("formats duration, bytes, tokens, and currency for compact surfaces", () => {
    expect(formatUsageDuration(59)).toBe("59s");
    expect(formatUsageDuration(7260)).toBe("2h 1m");
    expect(formatUsageBytes(1536)).toBe("1.50 KiB");
    expect(formatUsageTokens(1234567)).toBe("1,234,567");
    expect(formatUsageCost(15000, "USD")).toBe("$0.015");
  });

  it("formats a stable UTC billing period", () => {
    expect(formatUsagePeriod("2026-07-01T00:00:00Z", "2026-08-01T00:00:00Z")).toBe(
      "Jul 1, 2026 – Aug 1, 2026",
    );
  });

  it("clamps visual progress while retaining over-limit semantics in the source data", () => {
    expect(usageProgressPercent(8, 10)).toBe(80);
    expect(usageProgressPercent(11, 10)).toBe(100);
    expect(usageProgressPercent(11, null)).toBe(0);
  });

  it("groups retries and generations into explainable per-Turn totals", () => {
    const groups = groupSessionUsageByTurn([
      {
        executionId: "execution-1",
        generation: 1,
        turnId: "turn-1",
        provider: "codex",
        model: "gpt-5.6-sol",
        inputTokens: 10,
        cachedInputTokens: 0,
        outputTokens: 5,
        reasoningTokens: 0,
        totalTokens: 15,
        networkIngressBytes: 0,
        networkEgressBytes: 0,
        durationMillis: 1000,
        providerCostMicros: 0,
        providerCostReported: false,
        providerCurrency: "USD",
        platformCharges: [],
        totalCostByCurrency: {},
        costCoverage: "provider-unavailable",
        final: false,
        updatedAt: "2026-07-30T00:00:00Z",
      },
      {
        executionId: "execution-2",
        generation: 2,
        turnId: "turn-1",
        provider: "codex",
        model: "gpt-5.6-sol",
        inputTokens: 20,
        cachedInputTokens: 0,
        outputTokens: 10,
        reasoningTokens: 0,
        totalTokens: 30,
        networkIngressBytes: 0,
        networkEgressBytes: 0,
        durationMillis: 2000,
        providerCostMicros: 200,
        providerCostReported: true,
        providerCurrency: "USD",
        platformCharges: [{ kind: "cpu", currencyCode: "USD", amountMicros: 50, source: "actual" }],
        totalCostByCurrency: { USD: 250 },
        costCoverage: "provider-and-allocated-platform",
        final: true,
        updatedAt: "2026-07-30T00:01:00Z",
      },
    ]);

    expect(groups).toEqual([
      {
        turnId: "turn-1",
        providers: ["codex"],
        models: ["gpt-5.6-sol"],
        executionCount: 2,
        totalTokens: 45,
        networkIngressBytes: 0,
        networkEgressBytes: 0,
        durationMillis: 3000,
        providerCostByCurrency: { USD: 200 },
        providerCostReportedCount: 1,
        providerCostMissingCount: 1,
        platformCharges: [{ kind: "cpu", currencyCode: "USD", amountMicros: 50, source: "actual" }],
        platformCostByCurrency: { USD: 50 },
        totalCostByCurrency: { USD: 250 },
        final: false,
      },
    ]);
  });
});
