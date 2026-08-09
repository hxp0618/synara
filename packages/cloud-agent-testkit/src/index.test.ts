import { describe, expect, it } from "vitest";

import { CLOUD_AGENT_CAPABILITY_IDS } from "@synara/cloud-agent-protocol";
import {
  CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
  type CloudAgentProviderDescriptor,
} from "@synara/cloud-agent-provider-api";

import {
  assertCloudAgentDescriptor,
  assertCloudAgentTranscript,
  assertTerminalCorrelation,
  cloudAgentCommand,
} from "./index";

describe("cloud-agent testkit", () => {
  it("checks the complete provider descriptor surface", () => {
    const descriptor = fakeDescriptor();
    expect(() => assertCloudAgentDescriptor(descriptor)).not.toThrow();
  });

  it("checks terminal command correlation", () => {
    const command = cloudAgentCommand({ commandType: "Describe", commandId: "describe-1" });
    const terminal = {
      requestId: command.requestId,
      protocolVersion: command.protocolVersion,
      executionId: command.executionId,
      generation: command.generation,
      commandId: command.commandId,
      occurredAt: command.occurredAt,
      messageType: "Result" as const,
      payload: {},
    };
    expect(() => assertTerminalCorrelation(command, terminal)).not.toThrow();
  });

  it("rejects invalid capability values even when a caller bypasses TypeScript", () => {
    const descriptor = fakeDescriptor();
    expect(() =>
      assertCloudAgentDescriptor({
        ...descriptor,
        capabilities: { ...descriptor.capabilities, compact: "sometimes" },
      } as unknown as typeof descriptor),
    ).toThrow("invalid support");
  });

  it("checks multiplexed transcript correlation, event version, terminal, and late-frame rules", () => {
    const command = cloudAgentCommand({ commandType: "SendTurn", commandId: "turn-1" });
    const event = {
      requestId: command.requestId,
      protocolVersion: command.protocolVersion,
      executionId: command.executionId,
      generation: command.generation,
      commandId: command.commandId,
      occurredAt: command.occurredAt,
      messageType: "Event" as const,
      payload: { eventVersion: 2 as const, eventType: "turn.started" as const, payload: {} },
    };
    const terminal = { ...event, messageType: "Result" as const, payload: {} };

    expect(() => assertCloudAgentTranscript([command], [event, terminal])).not.toThrow();
    expect(() => assertCloudAgentTranscript([command], [terminal, event])).toThrow(
      "frame after terminal",
    );
    expect(() =>
      assertCloudAgentTranscript(
        [command],
        [{ ...event, payload: { ...event.payload, eventVersion: 1 as 2 } }, terminal],
      ),
    ).toThrow("eventVersion");
  });
});

function fakeDescriptor(): CloudAgentProviderDescriptor {
  return {
    abiVersion: CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
    providerKind: "test-provider",
    displayName: "Test Provider",
    adapterVersion: "test-adapter-v1",
    runtime: {
      kind: "local",
      name: "test-runtime",
      version: "1.0.0",
      available: true,
      compatible: true,
      compatibleRange: { minimumInclusive: "1.0.0", maximumExclusive: "2.0.0" },
    },
    capabilities: Object.fromEntries(
      CLOUD_AGENT_CAPABILITY_IDS.map((capability) => [capability, "unsupported"]),
    ) as CloudAgentProviderDescriptor["capabilities"],
    configurationSchema: { type: "object", additionalProperties: false },
  };
}
