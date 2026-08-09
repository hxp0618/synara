import providerCapabilityCatalog from "../provider-capability-catalog.json";

import type { CloudAgentCapabilityId, CloudAgentCapabilityMap } from "./protocol";

export interface CloudAgentProviderCapabilityCatalogEntry {
  readonly provider: string;
  readonly supportTier: "tier-1" | "tier-2" | "experimental" | "local-only";
  readonly adapterVersion: string;
  readonly runtimePolicy: {
    readonly kind: "cli" | "sdk" | "local";
    readonly name: string;
    readonly versionSource: "probe" | "package" | "build";
    readonly compatibleRange: {
      readonly minimumInclusive: string;
      readonly maximumExclusive?: string;
    };
  };
  readonly capabilities: CloudAgentCapabilityMap;
}

export interface CloudAgentProviderCapabilityCatalog {
  readonly version: 1;
  readonly capabilityIds: ReadonlyArray<CloudAgentCapabilityId>;
  readonly providers: ReadonlyArray<CloudAgentProviderCapabilityCatalogEntry>;
}

/** Single editable capability-catalog source shared by compatibility hosts. */
export const CLOUD_AGENT_PROVIDER_CAPABILITY_CATALOG =
  providerCapabilityCatalog as CloudAgentProviderCapabilityCatalog;
