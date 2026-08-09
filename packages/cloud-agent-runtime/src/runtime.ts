import {
  CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
  type CloudAgentHostServices,
  type CloudAgentProviderDescriptor,
  type CloudAgentProviderPluginV1,
  type CloudAgentProviderSession,
} from "@synara/cloud-agent-provider-api";

export interface CloudAgentRuntimeV1 {
  readonly abiVersion: typeof CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION;
  readonly providerKinds: ReadonlyArray<string>;
  describe(providerKind: string, signal?: AbortSignal): Promise<CloudAgentProviderDescriptor>;
  createSession(
    providerKind: string,
    input: {
      readonly hostInstanceId: string;
      readonly hostThreadId: string;
      readonly configuration: Readonly<Record<string, unknown>>;
    },
    host: CloudAgentHostServices,
    signal?: AbortSignal,
  ): Promise<CloudAgentProviderSession>;
}

export function createCloudAgentRuntime(input: {
  readonly providers: ReadonlyArray<CloudAgentProviderPluginV1>;
}): CloudAgentRuntimeV1 {
  const providers = new Map<string, CloudAgentProviderPluginV1>();
  for (const provider of input.providers) {
    if (provider.abiVersion !== CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION) {
      throw new Error(
        `Provider ${provider.providerKind} uses unsupported ABI ${provider.abiVersion}.`,
      );
    }
    const providerKind = normalizedProviderKind(provider.providerKind);
    if (providers.has(providerKind)) {
      throw new Error(`Provider ${providerKind} is registered more than once.`);
    }
    providers.set(providerKind, provider);
  }

  const providerKinds = Object.freeze([...providers.keys()].toSorted());

  const runtime: CloudAgentRuntimeV1 = {
    abiVersion: CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION,
    providerKinds,
    describe: (providerKind, signal) => providerFor(providers, providerKind).describe(signal),
    createSession: (providerKind, sessionInput, host, signal) =>
      providerFor(providers, providerKind).createSession(sessionInput, host, signal),
  };
  return Object.freeze(runtime);
}

function providerFor(
  providers: ReadonlyMap<string, CloudAgentProviderPluginV1>,
  providerKind: string,
): CloudAgentProviderPluginV1 {
  const normalized = normalizedProviderKind(providerKind);
  const provider = providers.get(normalized);
  if (!provider) throw new Error(`Provider ${normalized} is not registered.`);
  return provider;
}

function normalizedProviderKind(value: string): string {
  const normalized = value.trim();
  if (!normalized || normalized.length > 64 || !/^[a-zA-Z][a-zA-Z0-9_-]*$/u.test(normalized)) {
    throw new Error("Provider kind must be a non-empty portable slug.");
  }
  return normalized;
}
