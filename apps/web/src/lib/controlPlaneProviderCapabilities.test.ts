import type {
  ProviderCapabilityProjection,
  ProviderCapabilityProjectionItem,
  ProviderCapabilityProjectionStatus,
  ProviderKind,
} from "@synara/contracts";
import { describe, expect, it } from "vitest";

import {
  resolveControlPlaneCapabilityDecision,
  resolveControlPlaneTurnDispatchDecision,
} from "./controlPlaneProviderCapabilities";

function projection(
  provider: ProviderKind,
  overrides: Partial<ProviderCapabilityProjectionItem> = {},
): ProviderCapabilityProjection {
  const status: ProviderCapabilityProjectionStatus = overrides.status ?? "supported";
  return {
    executionTargetId: "target-1",
    targetKind: "kubernetes",
    basis: "target",
    items: [
      {
        provider,
        capabilityId: "send-turn",
        status,
        reasonCode: status === "supported" ? "capability_supported" : "capability_unsupported",
        ...(status === "supported" ? { supportMode: "native" as const } : {}),
        ...overrides,
      },
    ],
  };
}

describe("Control Plane Provider capability decisions", () => {
  it("leaves local mode unchanged without requiring a projection", () => {
    expect(
      resolveControlPlaneCapabilityDecision({
        isAuthoritative: false,
        projection: undefined,
        provider: "cursor",
        capabilityId: "send-turn",
      }),
    ).toMatchObject({ allowed: true, temporary: false, status: "local" });
  });

  it.each(["cursor", "gemini", "grok", "kilo", "opencode", "pi", "droid"] as const)(
    "blocks static local-only provider %s in SaaS mode",
    (provider) => {
      expect(
        resolveControlPlaneCapabilityDecision({
          isAuthoritative: true,
          projection: projection(provider),
          provider,
          capabilityId: "send-turn",
        }),
      ).toMatchObject({
        allowed: false,
        temporary: false,
        status: "unsupported",
        reasonCode: "capability_unsupported",
      });
    },
  );

  it("blocks statically unsupported advanced commands before consulting a Worker projection", () => {
    expect(
      resolveControlPlaneCapabilityDecision({
        isAuthoritative: true,
        projection: undefined,
        provider: "codex",
        capabilityId: "fork",
      }),
    ).toMatchObject({ allowed: false, status: "unsupported", temporary: false });
  });

  it("allows native and emulated projected support", () => {
    for (const supportMode of ["native", "emulated"] as const) {
      expect(
        resolveControlPlaneCapabilityDecision({
          isAuthoritative: true,
          projection: projection("codex", { supportMode }),
          provider: "codex",
          capabilityId: "send-turn",
        }),
      ).toMatchObject({ allowed: true, status: "supported", supportMode });
    }
  });

  it("blocks an explicit projection rejection with its stable reason", () => {
    expect(
      resolveControlPlaneCapabilityDecision({
        isAuthoritative: true,
        projection: projection("codex", {
          status: "unsupported",
          reasonCode: "provider_version_incompatible",
          supportMode: undefined,
        }),
        provider: "codex",
        capabilityId: "send-turn",
      }),
    ).toMatchObject({
      allowed: false,
      temporary: false,
      status: "unsupported",
      reasonCode: "provider_version_incompatible",
    });
  });

  it.each(["start-session", "send-turn", "plan-mode", "interrupt-turn"] as const)(
    "allows queue-safe unobserved capability %s while surfacing temporary status",
    (capabilityId) => {
      expect(
        resolveControlPlaneCapabilityDecision({
          isAuthoritative: true,
          projection: projection("codex", {
            capabilityId,
            status: "unobserved",
            reasonCode: "worker_manifest_required",
            supportMode: undefined,
          }),
          provider: "codex",
          capabilityId,
        }),
      ).toMatchObject({ allowed: true, temporary: true, status: "unobserved" });
    },
  );

  it("blocks unobserved steer capability", () => {
    expect(
      resolveControlPlaneCapabilityDecision({
        isAuthoritative: true,
        projection: projection("codex", {
          capabilityId: "steer-turn",
          status: "unobserved",
          reasonCode: "worker_manifest_required",
          supportMode: undefined,
        }),
        provider: "codex",
        capabilityId: "steer-turn",
      }),
    ).toMatchObject({ allowed: false, temporary: true, status: "unobserved" });
  });

  it("waits for an unresolved projection instead of racing Session creation", () => {
    expect(
      resolveControlPlaneTurnDispatchDecision({
        isAuthoritative: true,
        projection: undefined,
        provider: "codex",
        includeSessionStart: true,
        interactionMode: "default",
      }),
    ).toMatchObject({ allowed: false, temporary: true });
  });

  it("requires start, send, and plan before a first Plan Turn", () => {
    const items: ProviderCapabilityProjectionItem[] = [
      {
        provider: "codex",
        capabilityId: "start-session",
        status: "supported",
        reasonCode: "capability_supported",
        supportMode: "native",
      },
      {
        provider: "codex",
        capabilityId: "send-turn",
        status: "supported",
        reasonCode: "capability_supported",
        supportMode: "native",
      },
      {
        provider: "codex",
        capabilityId: "plan-mode",
        status: "unsupported",
        reasonCode: "capability_unsupported",
      },
    ];
    const decision = resolveControlPlaneTurnDispatchDecision({
      isAuthoritative: true,
      projection: {
        executionTargetId: "target-1",
        targetKind: "docker",
        basis: "target",
        items,
      },
      provider: "codex",
      includeSessionStart: true,
      interactionMode: "plan",
    });

    expect(decision.allowed).toBe(false);
    expect(decision.decisions.map((item) => item.capabilityId)).toEqual([
      "start-session",
      "send-turn",
      "plan-mode",
    ]);
    expect(decision.blockingDecision?.capabilityId).toBe("plan-mode");
  });
});
