import type { CloudAgentProviderPluginV1 } from "@synara/cloud-agent-provider-api";
import { createLegacyProviderPlugin } from "@synara/cloud-agent-runtime/legacy-provider-host";

export const CODEX_PROVIDER_KIND = "codex" as const;

export function createCodexProvider(): CloudAgentProviderPluginV1 {
  return createLegacyProviderPlugin({
    providerKind: CODEX_PROVIDER_KIND,
    displayName: "Codex",
    configurationSchema: {
      type: "object",
      additionalProperties: false,
      properties: {
        model: { type: "string", minLength: 1 },
      },
    },
  });
}
