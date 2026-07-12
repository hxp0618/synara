import {
  PROVIDER_CAPABILITY_IDS,
  PROVIDER_HOST_PROTOCOL_VERSION,
  type ProviderHostCommandEnvelope,
  type ProviderHostMessageEnvelope,
} from "@synara/contracts/provider-host";
import { describe, expect, it } from "vitest";

import {
  capabilityMapForProvider,
  createProviderHostProtocolHandler,
  providerHostDescriptor,
} from "./protocol";

function command(
  commandType: ProviderHostCommandEnvelope["commandType"],
  payload: Record<string, unknown>,
  commandId = `command-${commandType}`,
): ProviderHostCommandEnvelope {
  return {
    requestId: `request-${commandType}`,
    protocolVersion: PROVIDER_HOST_PROTOCOL_VERSION,
    executionId: "execution-1",
    generation: 1,
    commandType,
    commandId: commandId as ProviderHostCommandEnvelope["commandId"],
    occurredAt: "2026-07-13T02:00:00.000Z",
    payload,
  };
}

describe("Provider Host Protocol v2", () => {
  it("describes every capability and keeps unsupported Providers Local-only", () => {
    const codex = providerHostDescriptor("codex");
    const cursor = providerHostDescriptor("cursor");

    expect(Object.keys(codex.capabilityDescriptor.capabilities)).toHaveLength(
      PROVIDER_CAPABILITY_IDS.length,
    );
    expect(codex.capabilityDescriptor.capabilities["send-turn"]).toBe("native");
    expect(cursor.capabilityDescriptor.supportTier).toBe("local-only");
    expect(Object.values(capabilityMapForProvider("cursor"))).toEqual(
      Array(PROVIDER_CAPABILITY_IDS.length).fill("unsupported"),
    );
  });

  it("returns a versioned Describe result and replays the same terminal by commandId", async () => {
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
    });
    const describe = command("Describe", { provider: "codex" }, "describe-1");

    const first = await handle(describe);
    const second = await handle(describe);

    expect(first.at(-1)?.messageType).toBe("Result");
    expect(second).toEqual([first.at(-1)]);
    expect(emitted).toHaveLength(2);
  });

  it("rejects a Local-only Provider before execution", async () => {
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
    });
    const result = await handle(
      command("StartSession", {
        runnerInput: {
          execution: { id: "execution-1" },
          workload: { provider: "cursor", inputText: "unused" },
          workspaceDirectory: "/tmp/workspace",
        },
      }),
    );

    const terminal = result.at(-1);
    expect(terminal?.messageType).toBe("Error");
    if (terminal?.messageType === "Error") {
      expect(terminal.error.code).toBe("capability_unsupported");
    }
  });
});
