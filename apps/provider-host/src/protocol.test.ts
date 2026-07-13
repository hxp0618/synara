import { PROVIDER_RUNTIME_EVENT_VERSION } from "@synara/contracts";
import {
  PROVIDER_CAPABILITY_IDS,
  PROVIDER_HOST_PROTOCOL_VERSION,
  type ProviderHostCommandEnvelope,
  type ProviderHostMessageEnvelope,
} from "@synara/contracts/provider-host";
import { PassThrough } from "node:stream";
import { describe, expect, it } from "vitest";

import {
  capabilityMapForProvider,
  createProviderHostProtocolHandler,
  providerHostDescriptor,
  runProviderHostProtocolV2,
} from "./protocol";
import type { ProviderRunController, RunnerMessage } from "./providerHost";

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
    expect(codex.capabilityDescriptor.capabilities["steer-turn"]).toBe("native");
    expect(codex.capabilityDescriptor.capabilities["interrupt-turn"]).toBe("native");
    expect(codex.capabilityDescriptor.capabilities.approval).toBe("native");
    expect(codex.capabilityDescriptor.capabilities["structured-user-input"]).toBe("native");
    expect(codex.capabilityDescriptor.capabilities["plan-mode"]).toBe("native");
    expect(codex.runtimeEventVersions).toEqual({
      minimum: PROVIDER_RUNTIME_EVENT_VERSION,
      maximum: PROVIDER_RUNTIME_EVENT_VERSION,
    });
    const claude = providerHostDescriptor("claudeAgent");
    expect(claude.capabilityDescriptor.adapterVersion).toBe("claude-agent-sdk-v2");
    expect(claude.capabilityDescriptor.capabilities["steer-turn"]).toBe("native");
    expect(claude.capabilityDescriptor.capabilities["interrupt-turn"]).toBe("native");
    expect(claude.capabilityDescriptor.capabilities.approval).toBe("native");
    expect(claude.capabilityDescriptor.capabilities["structured-user-input"]).toBe("native");
    expect(claude.capabilityDescriptor.capabilities["plan-mode"]).toBe("native");
    expect(claude.capabilityDescriptor.capabilities["tool-events"]).toBe("native");
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

  it("processes InterruptTurn while SendTurn is still active", async () => {
    let rejectRun: ((error: Error) => void) | undefined;
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      startRun: () =>
        ({
          result: new Promise((_, reject) => {
            rejectRun = reject;
          }),
          interrupt: () => rejectRun?.(new Error("Provider turn was interrupted.")),
          getResumeCursor: () => "provider-cursor-after-interrupt",
        }) satisfies ProviderRunController,
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-interrupt"));

    const send = handle(command("SendTurn", { inputText: "long task" }, "send-interrupt"));
    const interrupt = await handle(
      command("InterruptTurn", { targetCommandId: "send-interrupt" }, "interrupt-active"),
    );
    const sendMessages = await send;

    expect(interrupt.at(-1)).toMatchObject({
      messageType: "Result",
      payload: {
        interrupted: true,
        targetCommandId: "send-interrupt",
        providerResumeCursor: "provider-cursor-after-interrupt",
      },
    });
    expect(sendMessages.at(-1)).toMatchObject({
      messageType: "Error",
      error: { code: "interrupted" },
    });
  });

  it("emits only canonical Runtime Event v2 payloads on the v2 wire", async () => {
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
      startRun: (_input, _credential, emit) => {
        emit({
          type: "event",
          eventType: "runtime.output.delta",
          payload: { text: "canonical" },
        });
        return {
          result: new Promise((resolve) => {
            completeRun = resolve;
          }),
          interrupt: () => {},
        } satisfies ProviderRunController;
      },
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-events"));

    const send = handle(command("SendTurn", { inputText: "stream" }, "send-events"));
    completeRun?.({ type: "result", output: { text: "canonical" } });
    await send;

    expect(emitted).toContainEqual(
      expect.objectContaining({
        commandId: "send-events",
        messageType: "Event",
        payload: {
          eventVersion: PROVIDER_RUNTIME_EVENT_VERSION,
          eventType: "content.delta",
          payload: { streamKind: "assistant_text", delta: "canonical" },
        },
      }),
    );
  });

  it("processes SteerTurn while SendTurn remains active", async () => {
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    let steeredPayload: Record<string, unknown> | undefined;
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: () => {},
      startRun: () =>
        ({
          result: new Promise((resolve) => {
            completeRun = resolve;
          }),
          interrupt: () => {},
          steer: (payload) => {
            steeredPayload = payload;
          },
        }) satisfies ProviderRunController,
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-steer"));

    const send = handle(command("SendTurn", { inputText: "long task" }, "send-steer"));
    const steer = await handle(
      command(
        "SteerTurn",
        { targetCommandId: "send-steer", inputText: "focus on tests" },
        "steer-active",
      ),
    );
    completeRun?.({ type: "result", output: { text: "done" } });
    const sendMessages = await send;

    expect(steeredPayload).toEqual({ inputText: "focus on tests" });
    expect(steer.at(-1)).toMatchObject({
      messageType: "Result",
      payload: { steered: true, targetCommandId: "send-steer" },
    });
    expect(sendMessages.at(-1)).toMatchObject({
      messageType: "Result",
      payload: { output: { text: "done" } },
    });
  });

  it("delivers a correlated approval resolution during an active SendTurn", async () => {
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    let resolvedPayload: Record<string, unknown> | undefined;
    const emitted: ProviderHostMessageEnvelope[] = [];
    const handle = createProviderHostProtocolHandler({
      credential: null,
      emit: (message) => emitted.push(message),
      startRun: (_input, _credential, emit) => {
        emit({
          type: "interaction",
          interactionType: "approval",
          payload: { requestId: "approval-1", summary: "Run command" },
        });
        return {
          result: new Promise((resolve) => {
            completeRun = resolve;
          }),
          interrupt: () => {},
          resolveApproval: (payload) => {
            resolvedPayload = payload;
            completeRun?.({ type: "result", output: { text: "approved" } });
          },
        } satisfies ProviderRunController;
      },
    });
    await handle(command("StartSession", { runnerInput: remoteRunnerInput() }, "session-approval"));

    const send = handle(command("SendTurn", { inputText: "needs approval" }, "send-approval"));
    const resolution = await handle(
      command(
        "ResolveApproval",
        { requestId: "approval-1", resolution: { decision: "accept" } },
        "approval-1:resolution",
      ),
    );
    const sendMessages = await send;

    expect(emitted).toContainEqual(
      expect.objectContaining({
        commandId: "send-approval",
        messageType: "InteractionRequest",
        payload: expect.objectContaining({ interactionType: "approval", requestId: "approval-1" }),
      }),
    );
    expect(resolvedPayload).toEqual({
      requestId: "approval-1",
      resolution: { decision: "accept" },
    });
    expect(resolution.at(-1)?.messageType).toBe("Result");
    expect(sendMessages.at(-1)).toMatchObject({
      messageType: "Result",
      payload: { output: { text: "approved" } },
    });
  });

  it("keeps reading stdin commands while SendTurn is pending", async () => {
    const source = new PassThrough();
    const emitted: ProviderHostMessageEnvelope[] = [];
    let completeRun: ((message: Extract<RunnerMessage, { type: "result" }>) => void) | undefined;
    const protocol = runProviderHostProtocolV2({
      source,
      credential: null,
      emit: (message) => emitted.push(message),
      startRun: (_input, _credential, emit) => {
        emit({
          type: "interaction",
          interactionType: "approval",
          payload: { requestId: "approval-stream" },
        });
        return {
          result: new Promise((resolve) => {
            completeRun = resolve;
          }),
          interrupt: () => {},
          resolveApproval: () => {
            completeRun?.({ type: "result", output: { text: "stream-approved" } });
          },
        } satisfies ProviderRunController;
      },
    });

    for (const item of [
      command("StartSession", { runnerInput: remoteRunnerInput() }, "session-stream"),
      command("SendTurn", { inputText: "stream task" }, "send-stream"),
      command(
        "ResolveApproval",
        { requestId: "approval-stream", resolution: { decision: "accept" } },
        "approval-stream:resolution",
      ),
    ]) {
      source.write(`${JSON.stringify(item)}\n`);
    }
    source.end();
    await protocol;

    expect(emitted).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ commandId: "approval-stream:resolution", messageType: "Result" }),
        expect.objectContaining({
          commandId: "send-stream",
          messageType: "Result",
          payload: { output: { text: "stream-approved" } },
        }),
      ]),
    );
  });
});

function remoteRunnerInput() {
  return {
    execution: { id: "execution-1" },
    workload: { provider: "codex", inputText: "initial" },
    workspaceDirectory: "/tmp/workspace",
  };
}
