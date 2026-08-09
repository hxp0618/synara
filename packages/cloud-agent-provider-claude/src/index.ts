import type { CloudAgentProviderPluginV1 } from "@synara/cloud-agent-provider-api";
import { createLegacyProviderPlugin } from "@synara/cloud-agent-runtime/legacy-provider-host";

export const CLAUDE_PROVIDER_KIND = "claudeAgent" as const;

export function createClaudeProvider(): CloudAgentProviderPluginV1 {
  return createLegacyProviderPlugin({
    providerKind: CLAUDE_PROVIDER_KIND,
    displayName: "Claude",
    configurationSchema: {
      type: "object",
      additionalProperties: false,
      properties: {
        model: { type: "string", minLength: 1 },
      },
    },
  });
}
