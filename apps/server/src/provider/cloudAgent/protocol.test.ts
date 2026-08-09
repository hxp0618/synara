import { describe, expect, it } from "vitest";
import type { ProviderSessionStartInput, ThreadId } from "@synara/contracts";

import {
  assertCloudAgentCodexDescriptor,
  cloudAgentRunnerInput,
  decodeCloudAgentCursor,
  encodeCloudAgentCursor,
  makeCloudAgentCommand,
} from "./protocol.ts";

const capabilities = {
  "start-session": "native",
  "send-turn": "native",
  "interrupt-turn": "native",
  approval: "native",
  "structured-user-input": "native",
};
const threadId = "thread-1" as ThreadId;

describe("Cloud Agent Codex protocol mapping", () => {
  it("builds Protocol v2 commands and validates the selected descriptor", () => {
    const command = makeCloudAgentCommand({
      executionId: "execution-1",
      generation: 1,
      commandType: "Describe",
      payload: { provider: "codex" },
    });
    expect(command).toMatchObject({
      protocolVersion: { major: 2 },
      executionId: "execution-1",
      generation: 1,
      commandType: "Describe",
    });
    expect(() =>
      assertCloudAgentCodexDescriptor({
        providerKind: "codex",
        runtime: { available: true, compatible: true },
        capabilities,
      }),
    ).not.toThrow();
    expect(() =>
      assertCloudAgentCodexDescriptor({
        providerKind: "codex",
        runtime: { available: true, compatible: true },
        capabilities: { ...capabilities, "send-turn": "unsupported" },
      }),
    ).toThrow("required capability");
  });

  it("accepts only this adapter's opaque resume cursor", () => {
    const cursor = encodeCloudAgentCursor("provider-thread-1");
    expect(decodeCloudAgentCursor(cursor)).toEqual(cursor);
    expect(() => decodeCloudAgentCursor("provider-thread-1")).toThrow(
      "was not created by CloudAgentCodexAdapter",
    );
    expect(() =>
      decodeCloudAgentCursor({
        kind: "native-codex",
        version: 1,
        providerResumeCursor: "provider-thread-1",
      }),
    ).toThrow("was not created by CloudAgentCodexAdapter");
  });

  it("fails closed on unrepresentable runtime and model options", () => {
    expect(() =>
      cloudAgentRunnerInput({
        threadId,
        provider: "codex",
        runtimeMode: "auto",
      } satisfies ProviderSessionStartInput),
    ).toThrow("not representable");
    expect(() =>
      cloudAgentRunnerInput({
        threadId,
        provider: "codex",
        runtimeMode: "full-access",
        modelSelection: {
          provider: "codex",
          model: "gpt-5.5",
          options: { reasoningEffort: "high" },
        },
      } satisfies ProviderSessionStartInput),
    ).toThrow("model options");
  });
});
