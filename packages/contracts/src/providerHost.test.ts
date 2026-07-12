import { Schema } from "effect";
import { describe, expect, it } from "vitest";

import {
  PROVIDER_CAPABILITY_IDS,
  PROVIDER_HOST_PROTOCOL_VERSION,
  ProviderHostCommandEnvelope,
  ProviderHostDescriptor,
} from "./providerHost";

const decodeDescriptor = Schema.decodeUnknownSync(ProviderHostDescriptor);
const decodeCommand = Schema.decodeUnknownSync(ProviderHostCommandEnvelope);

function completeCapabilities(value: "native" | "emulated" | "unsupported") {
  return Object.fromEntries(PROVIDER_CAPABILITY_IDS.map((capability) => [capability, value]));
}

describe("Provider Host v2 contracts", () => {
  it("decodes a complete capability descriptor", () => {
    const descriptor = decodeDescriptor({
      protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
      hostBuildVersion: "host-test",
      capabilityDescriptor: {
        provider: "codex",
        supportTier: "tier-1",
        adapterVersion: "codex-remote-v2",
        providerCliVersion: "0.144.1",
        capabilities: completeCapabilities("native"),
      },
      maximumCommandBytes: 2_097_152,
      maximumMessageBytes: 1_048_576,
      runtimeEventVersions: { minimum: 1, maximum: 2 },
      credentialDeliveryModes: ["anonymous-fd"],
      resumeStrategies: ["native-cursor", "authoritative-history"],
    });

    expect(descriptor.capabilityDescriptor.capabilities["send-turn"]).toBe("native");
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
});
