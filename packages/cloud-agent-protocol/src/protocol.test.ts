import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";
import { describe, expect, it } from "vitest";

import envelopeSchema from "../schemas/cloud-agent-envelope-v2.schema.json";

import {
  CLOUD_AGENT_CAPABILITY_IDS,
  CLOUD_AGENT_COMMAND_TYPES,
  CLOUD_AGENT_ERROR_CODES,
  CLOUD_AGENT_MAX_COMMAND_BYTES,
  CLOUD_AGENT_MAX_MESSAGE_BYTES,
  CLOUD_AGENT_PROTOCOL_VERSION,
  CLOUD_AGENT_RUNTIME_EVENT_TYPES,
  CLOUD_AGENT_RUNTIME_EVENT_VERSION,
} from "./index";

const ajv = new Ajv2020({ allErrors: true, strict: true });
addFormats(ajv);
const validateEnvelope = ajv.compile(envelopeSchema);
const envelopeBase = {
  requestId: "request-1",
  protocolVersion: CLOUD_AGENT_PROTOCOL_VERSION,
  executionId: "execution-1",
  generation: 1,
  commandId: "command-1",
  occurredAt: "2026-08-09T00:00:00.000Z",
};

const validCommandPayloads: Record<(typeof CLOUD_AGENT_COMMAND_TYPES)[number], object> = {
  Describe: { provider: "codex" },
  StartSession: { runnerInput: {} },
  ResumeSession: { runnerInput: {} },
  SendTurn: { inputText: "hello" },
  SteerTurn: { inputText: "continue", targetCommandId: "turn-1" },
  InterruptTurn: { targetCommandId: "turn-1" },
  SuspendTurn: { targetCommandId: "turn-1" },
  ResolveApproval: { requestId: "approval-1", resolution: { decision: "accept" } },
  ResolveUserInput: { requestId: "input-1", resolution: { answers: { scope: "repo" } } },
  CompactSession: {},
  RollbackSession: {},
  ForkSession: {},
  StartReview: {},
  GenerateText: { task: "thread-title", input: { prompt: "hello" } },
  StopSession: {},
};

describe("cloud-agent protocol constants", () => {
  it("pins the additive Provider Host v2.3 wire limits", () => {
    expect(CLOUD_AGENT_PROTOCOL_VERSION).toEqual({ major: 2, minor: 3 });
    expect(CLOUD_AGENT_RUNTIME_EVENT_VERSION).toBe(2);
    expect(CLOUD_AGENT_MAX_COMMAND_BYTES).toBe(2 * 1024 * 1024);
    expect(CLOUD_AGENT_MAX_MESSAGE_BYTES).toBe(1024 * 1024);
  });

  it("keeps public vocabularies duplicate-free", () => {
    for (const values of [
      CLOUD_AGENT_CAPABILITY_IDS,
      CLOUD_AGENT_COMMAND_TYPES,
      CLOUD_AGENT_ERROR_CODES,
      CLOUD_AGENT_RUNTIME_EVENT_TYPES,
    ]) {
      expect(new Set(values).size).toBe(values.length);
    }
  });

  it("keeps every public command, error, and Runtime Event vocabulary valid in JSON Schema", () => {
    for (const commandType of CLOUD_AGENT_COMMAND_TYPES) {
      expect(
        validateEnvelope({
          ...envelopeBase,
          commandType,
          payload: validCommandPayloads[commandType],
        }),
        `${commandType}: ${JSON.stringify(validateEnvelope.errors)}`,
      ).toBe(true);
    }
    for (const code of CLOUD_AGENT_ERROR_CODES) {
      expect(
        validateEnvelope({
          ...envelopeBase,
          messageType: "Error",
          error: {
            code,
            message: "test error",
            retryable: false,
            requiresNewExecution: false,
            requiresUserAction: false,
            canReconstructFromHistory: true,
            canMoveWorker: true,
          },
        }),
        `${code}: ${JSON.stringify(validateEnvelope.errors)}`,
      ).toBe(true);
    }
    for (const eventType of CLOUD_AGENT_RUNTIME_EVENT_TYPES) {
      expect(
        validateEnvelope({
          ...envelopeBase,
          messageType: "Event",
          payload: { eventVersion: 2, eventType, payload: {} },
        }),
        `${eventType}: ${JSON.stringify(validateEnvelope.errors)}`,
      ).toBe(true);
    }
  });

  it("rejects message discriminators whose required body is missing or malformed", () => {
    for (const invalid of [
      { ...envelopeBase, messageType: "Result" },
      { ...envelopeBase, messageType: "Error", payload: {} },
      {
        ...envelopeBase,
        messageType: "Event",
        payload: { eventVersion: 1, eventType: "runtime.warning", payload: {} },
      },
      {
        ...envelopeBase,
        commandType: "ResolveApproval",
        payload: { requestId: "approval-1", decision: "accept" },
      },
    ]) {
      expect(validateEnvelope(invalid)).toBe(false);
    }
  });
});
