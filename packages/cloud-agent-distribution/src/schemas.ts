import cloudAgentEnvelopeV2Schema from "@synara/cloud-agent-protocol/schemas/cloud-agent-envelope-v2.schema.json" with { type: "json" };

export const CLOUD_AGENT_ENVELOPE_V2_SCHEMA = deepFreeze(cloudAgentEnvelopeV2Schema);

function deepFreeze<T>(value: T): Readonly<T> {
  if (value === null || typeof value !== "object" || Object.isFrozen(value)) return value;
  for (const child of Object.values(value)) deepFreeze(child);
  return Object.freeze(value);
}
