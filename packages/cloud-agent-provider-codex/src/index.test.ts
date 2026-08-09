import { describe, expect, it, vi } from "vitest";

vi.mock("node:child_process", async (importOriginal) => {
  const actual = await importOriginal<typeof import("node:child_process")>();
  return {
    ...actual,
    spawnSync: vi.fn(() => ({ status: 0, stdout: "codex-cli 0.145.0", stderr: "" })),
  };
});

vi.mock("./codexAppServerRuntime", () => ({
  startCodexAppServerRun: vi.fn(() => ({
    result: Promise.resolve({
      type: "result",
      output: { text: '{"task":"thread-title","title":"Generated title"}' },
    }),
    interrupt: () => undefined,
  })),
}));

import { createCodexProvider } from "./index";
import { startCodexAppServerRun } from "./codexAppServerRuntime";

process.env.SYNARA_PROVIDER_OUTER_SANDBOX_PROFILE = "single-tenant-trusted-v1";
process.env.SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS = "codex";

describe("createCodexProvider", () => {
  it("publishes a host-neutral ABI descriptor", async () => {
    const provider = createCodexProvider();
    const descriptor = await provider.describe();
    expect(provider.providerKind).toBe("codex");
    expect(descriptor.providerKind).toBe("codex");
    expect(descriptor.adapterVersion).toBe("codex-app-server-v2");
    expect(descriptor.runtime.name).toBe("codex");
  });

  it("injects the immutable default hook and no-tool marker for GenerateText", async () => {
    const provider = createCodexProvider();
    const session = await provider.createSession(
      { hostInstanceId: "instance-1", hostThreadId: "thread-1", configuration: {} },
      {
        workspace: {
          authority: "host",
          root: "/tmp/cloud-agent-provider-codex-index-test",
          generation: 1,
          readOnly: false,
        },
        credential: { acquire: async () => null },
        log: {
          debug: () => undefined,
          info: () => undefined,
          warn: () => undefined,
          error: () => undefined,
        },
      },
    );
    try {
      const base = {
        requestId: "request-1",
        protocolVersion: { major: 2, minor: 3 },
        executionId: "execution-1",
        generation: 1,
        occurredAt: "2026-08-09T00:00:00.000Z",
      } as const;
      const startResult = await session.execute({
        ...base,
        commandType: "StartSession",
        commandId: "start-1",
        payload: {
          runnerInput: {
            execution: { id: "execution-1" },
            workload: { provider: "codex", inputText: "start" },
            workspaceDirectory: "/tmp/cloud-agent-provider-codex-index-test",
          },
        },
      });
      const generateResult = await session.execute({
        ...base,
        commandType: "GenerateText",
        commandId: "generate-1",
        payload: { task: "thread-title", input: { message: "hello" } },
      });
      expect(startResult.messageType).toBe("Result");
      expect(generateResult.messageType).toBe("Result");
      const call = vi.mocked(startCodexAppServerRun).mock.calls.at(-1)?.[0];
      expect(call?.toolPolicyHookCommand).toContain(" -e ");
      expect(call?.environment.SYNARA_CODEX_NO_TOOL_OPERATION).toBe("1");
      expect((await session.events[Symbol.asyncIterator]().next()).value).toBeDefined();
    } finally {
      await session.close();
    }
  });
});
