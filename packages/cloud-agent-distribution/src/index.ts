import { createClaudeProvider } from "@synara/cloud-agent-provider-claude";
import { createCodexProvider } from "@synara/cloud-agent-provider-codex";
import { createCloudAgentRuntime, createCloudAgentStdioClient } from "@synara/cloud-agent-runtime";

import manifest from "../manifest.json";

export { createCloudAgentStdioClient };
export const CLOUD_AGENT_DISTRIBUTION_MANIFEST = deepFreeze(manifest);

export function createDefaultCloudAgentRuntime(
  options: { readonly toolPolicyHookCommand?: string } = {},
) {
  return createCloudAgentRuntime({
    providers: [createCodexProvider(options), createClaudeProvider()],
  });
}

function deepFreeze<T>(value: T): Readonly<T> {
  if (value === null || typeof value !== "object" || Object.isFrozen(value)) return value;
  for (const child of Object.values(value)) deepFreeze(child);
  return Object.freeze(value);
}
