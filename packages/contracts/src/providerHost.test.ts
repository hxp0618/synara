import { Schema } from "effect";
import { describe, expect, it } from "vitest";

import {
  PROVIDER_CAPABILITY_CATALOG,
  PROVIDER_CAPABILITY_IDS,
  PROVIDER_HOST_PROTOCOL_VERSION,
  PROVIDER_HOST_PROVIDER_KINDS,
  ProviderCapabilityProjection,
  ProviderHostCommandEnvelope,
  ProviderHostDescriptor,
  ProviderHostMessageEnvelope,
} from "./providerHost";
import { PROVIDER_RUNTIME_EVENT_VERSION } from "./providerRuntime";

const decodeDescriptor = Schema.decodeUnknownSync(ProviderHostDescriptor);
const decodeCommand = Schema.decodeUnknownSync(ProviderHostCommandEnvelope);
const decodeMessage = Schema.decodeUnknownSync(ProviderHostMessageEnvelope);
const decodeCapabilityProjection = Schema.decodeUnknownSync(ProviderCapabilityProjection);

function completeCapabilities(value: "native" | "emulated" | "unsupported") {
  return Object.fromEntries(PROVIDER_CAPABILITY_IDS.map((capability) => [capability, value]));
}

describe("Provider Host v2 contracts", () => {
  it("freezes the ordered 8 Provider by 28 Capability catalog without Droid", () => {
    expect(PROVIDER_HOST_PROVIDER_KINDS).toEqual([
      "codex",
      "claudeAgent",
      "cursor",
      "antigravity",
      "grok",
      "kilo",
      "opencode",
      "pi",
    ]);
    expect(PROVIDER_HOST_PROVIDER_KINDS).not.toContain("droid");
    expect(PROVIDER_CAPABILITY_IDS).toEqual([
      "discovery",
      "start-session",
      "resume-session",
      "send-turn",
      "steer-turn",
      "interrupt-turn",
      "approval",
      "structured-user-input",
      "plan-mode",
      "review",
      "compact",
      "rollback",
      "fork",
      "read-history",
      "model-list",
      "model-switch",
      "skill-discovery",
      "skill-mentions",
      "plugin-discovery",
      "plugin-mentions",
      "native-commands",
      "tool-events",
      "diff-events",
      "usage-events",
      "checkpoint",
      "credential-injection",
      "authoritative-history-reconstruction",
      "worker-migration",
    ]);
    expect(PROVIDER_CAPABILITY_CATALOG.version).toBe(1);
    expect(PROVIDER_CAPABILITY_CATALOG.capabilityIds).toEqual(PROVIDER_CAPABILITY_IDS);
    expect(PROVIDER_CAPABILITY_CATALOG.providers.map(({ provider }) => provider)).toEqual(
      PROVIDER_HOST_PROVIDER_KINDS,
    );
    expect(PROVIDER_CAPABILITY_CATALOG.providers.map(({ supportTier }) => supportTier)).toEqual([
      "experimental",
      "experimental",
      "local-only",
      "local-only",
      "local-only",
      "local-only",
      "local-only",
      "local-only",
    ]);

    for (const entry of PROVIDER_CAPABILITY_CATALOG.providers) {
      expect(Object.keys(entry.capabilities)).toEqual(PROVIDER_CAPABILITY_IDS);
      expect(Object.values(entry.capabilities)).toHaveLength(PROVIDER_CAPABILITY_IDS.length);
    }
  });

  it("decodes a complete capability descriptor", () => {
    const descriptor = decodeDescriptor({
      protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
      hostBuildVersion: "host-test",
      capabilityDescriptor: {
        provider: "codex",
        supportTier: "tier-1",
        adapterVersion: "codex-remote-v2",
        providerCliVersion: "0.145.0",
        runtime: {
          kind: "cli",
          name: "codex",
          version: "0.145.0",
          available: true,
          versionSource: "probe",
          compatibleRange: {
            minimumInclusive: "0.145.0",
            maximumExclusive: "0.146.0",
          },
          compatible: true,
        },
        releasePolicy: {
          requiresExplicitEnablement: true,
          enabled: true,
        },
        capabilities: completeCapabilities("native"),
      },
      maximumCommandBytes: 2_097_152,
      maximumMessageBytes: 1_048_576,
      runtimeEventVersions: { minimum: 1, maximum: 2 },
      credentialDeliveryModes: ["anonymous-fd"],
      resumeStrategies: ["native-cursor", "authoritative-history"],
    });

    expect(descriptor.capabilityDescriptor.capabilities["send-turn"]).toBe("native");
    expect(descriptor.protocolVersion).toEqual({ major: 2, minor: 1 });
    expect(descriptor.capabilityDescriptor.runtime.versionSource).toBe("probe");
  });

  it("marks SaaS model-switch as emulated for Codex and Claude Agent", () => {
    const providers = new Map(
      PROVIDER_CAPABILITY_CATALOG.providers.map((entry) => [entry.provider, entry] as const),
    );

    expect(providers.get("codex")?.capabilities["model-switch"]).toBe("emulated");
    expect(providers.get("claudeAgent")?.capabilities["model-switch"]).toBe("emulated");
  });

  it("advertises the implemented advanced Provider operations", () => {
    const providers = new Map(
      PROVIDER_CAPABILITY_CATALOG.providers.map((entry) => [entry.provider, entry] as const),
    );

    expect(providers.get("codex")?.capabilities).toMatchObject({
      review: "native",
      compact: "native",
      rollback: "unsupported",
      fork: "unsupported",
    });
    expect(providers.get("claudeAgent")?.capabilities).toMatchObject({
      review: "emulated",
      compact: "unsupported",
      rollback: "unsupported",
      fork: "unsupported",
    });
  });

  it("decodes a sanitized Target and Execution capability projection including Droid", () => {
    const targetProjection = decodeCapabilityProjection({
      executionTargetId: "target-1",
      targetKind: "kubernetes",
      basis: "target",
      items: [
        {
          provider: "codex",
          capabilityId: "send-turn",
          status: "supported",
          reasonCode: null,
          supportMode: "native",
        },
        {
          provider: "droid",
          capabilityId: "send-turn",
          status: "unsupported",
          reasonCode: "capability_unsupported",
        },
      ],
    });
    const executionProjection = decodeCapabilityProjection({
      executionTargetId: "target-1",
      targetKind: "kubernetes",
      executionId: "execution-1",
      basis: "execution",
      items: [
        {
          provider: "claudeAgent",
          capabilityId: "interrupt-turn",
          status: "unobserved",
          reasonCode: "worker_manifest_required",
        },
      ],
    });

    expect(targetProjection.items[0]?.supportMode).toBe("native");
    expect(targetProjection.items[1]?.provider).toBe("droid");
    expect(executionProjection.executionId).toBe("execution-1");
    expect(executionProjection.items[0]?.status).toBe("unobserved");
  });

  it("rejects invalid capability projection states and support modes", () => {
    const projection = {
      executionTargetId: "target-1",
      targetKind: "kubernetes",
      basis: "target",
      items: [
        {
          provider: "codex",
          capabilityId: "send-turn",
          status: "available",
          reasonCode: null,
        },
      ],
    };
    expect(() => decodeCapabilityProjection(projection)).toThrow();
    expect(() =>
      decodeCapabilityProjection({
        ...projection,
        items: [
          {
            provider: "codex",
            capabilityId: "send-turn",
            status: "supported",
            reasonCode: "capability_supported",
            supportMode: "unsupported",
          },
        ],
      }),
    ).toThrow();
  });

  it("accepts unknown optional fields from a newer compatible minor", () => {
    expect(() =>
      decodeDescriptor({
        protocolVersion: { major: 2, minor: 2 },
        hostBuildVersion: "host-test",
        futureOptionalField: { enabled: true },
        capabilityDescriptor: {
          provider: "claudeAgent",
          supportTier: "experimental",
          adapterVersion: "claude-agent-sdk-v2",
          futureProviderField: "ignored",
          runtime: {
            kind: "sdk",
            name: "@anthropic-ai/claude-agent-sdk",
            version: "0.3.207",
            available: true,
            versionSource: "package",
            compatibleRange: {
              minimumInclusive: "0.3.207",
              maximumExclusive: "0.4.0",
            },
            compatible: true,
          },
          releasePolicy: {
            requiresExplicitEnablement: true,
            enabled: false,
          },
          capabilities: completeCapabilities("native"),
        },
        maximumCommandBytes: 2_097_152,
        maximumMessageBytes: 1_048_576,
        runtimeEventVersions: { minimum: 2, maximum: 2 },
        credentialDeliveryModes: ["anonymous-fd"],
        resumeStrategies: ["native-cursor"],
      }),
    ).not.toThrow();
  });

  it("rejects a descriptor that omits a capability", () => {
    const capabilities = completeCapabilities("unsupported");
    delete capabilities["worker-migration"];

    expect(() =>
      decodeDescriptor({
        protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
        hostBuildVersion: "host-test",
        capabilityDescriptor: {
          provider: "cursor",
          supportTier: "local-only",
          adapterVersion: "none",
          runtime: {
            kind: "local",
            name: "cursor",
            version: "0.2.0-dev",
            available: true,
            versionSource: "build",
            compatibleRange: { minimumInclusive: "0.0.0" },
            compatible: true,
          },
          releasePolicy: {
            requiresExplicitEnablement: false,
            enabled: true,
          },
          capabilities,
        },
        maximumCommandBytes: 2_097_152,
        maximumMessageBytes: 1_048_576,
        runtimeEventVersions: { minimum: 1, maximum: 2 },
        credentialDeliveryModes: ["anonymous-fd"],
        resumeStrategies: ["authoritative-history"],
      }),
    ).toThrow();
  });

  it("decodes a versioned command envelope", () => {
    const command = decodeCommand({
      requestId: "request-1",
      protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
      executionId: "execution-1",
      generation: 2,
      commandType: "SendTurn",
      commandId: "command-1",
      occurredAt: "2026-07-13T02:00:00.000Z",
      payload: { inputText: "hello" },
    });

    expect(command.commandType).toBe("SendTurn");
  });

  it("accepts only canonical Runtime Event v2 payloads on Event messages", () => {
    const base = {
      requestId: "request-1",
      protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
      executionId: "execution-1",
      generation: 2,
      commandId: "command-1",
      occurredAt: "2026-07-13T02:00:00.000Z",
      messageType: "Event",
    } as const;

    expect(
      decodeMessage({
        ...base,
        payload: {
          eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
          eventType: "content.delta",
          payload: { streamKind: "assistant_text", delta: "hello" },
        },
      }).messageType,
    ).toBe("Event");

    expect(() =>
      decodeMessage({
        ...base,
        payload: {
          eventVersion: 1,
          eventType: "runtime.output.delta",
          payload: { text: "legacy" },
        },
      }),
    ).toThrow();
  });
});
