import type {
  CloudAgentCapabilityMap,
  CloudAgentCommandEnvelope,
  CloudAgentMessageEnvelope,
  CloudAgentTextGenerationTask,
} from "@synara/cloud-agent-protocol";

export const CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION = 1 as const;

export interface CloudAgentProviderRuntimeDescriptor {
  readonly kind: "cli" | "sdk" | "local";
  readonly name: string;
  readonly version?: string;
  readonly available: boolean;
  readonly compatible: boolean;
  readonly compatibleRange: {
    readonly minimumInclusive: string;
    readonly maximumExclusive?: string;
  };
}

export interface CloudAgentProviderDescriptor {
  readonly abiVersion: typeof CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION;
  readonly providerKind: string;
  readonly displayName: string;
  readonly adapterVersion: string;
  readonly runtime: CloudAgentProviderRuntimeDescriptor;
  readonly capabilities: CloudAgentCapabilityMap;
  readonly configurationSchema?: Readonly<Record<string, unknown>>;
  readonly textGenerationTasks?: ReadonlyArray<CloudAgentTextGenerationTask>;
}

export interface CloudAgentWorkspaceBinding {
  readonly authority: "host" | "external-readonly";
  readonly root: string | null;
  readonly runtimeOutputRoot?: string;
  readonly providerStateRoot?: string;
  readonly generation: number;
  readonly readOnly: boolean;
}

export interface CloudAgentCredentialLease extends AsyncDisposable {
  readonly delivery: "anonymous-fd";
  readonly fd: number;
  readonly expiresAt?: string;
}

export interface CloudAgentCredentialSource {
  acquire(input: {
    readonly providerKind: string;
    readonly operation: string;
    readonly generation: number;
    readonly signal?: AbortSignal;
  }): Promise<CloudAgentCredentialLease | null>;
}

export interface CloudAgentArtifactCandidate {
  readonly sourceRoot: "workspace" | "runtime-output";
  readonly relativePath: string;
  readonly kind: "diff" | "generated-file" | "terminal-log" | "provider-output";
  readonly contentType: string;
  readonly reportedSize?: number;
  readonly sha256?: string;
}

export interface CloudAgentLogger {
  debug(message: string, attributes?: Readonly<Record<string, unknown>>): void;
  info(message: string, attributes?: Readonly<Record<string, unknown>>): void;
  warn(message: string, attributes?: Readonly<Record<string, unknown>>): void;
  error(message: string, attributes?: Readonly<Record<string, unknown>>): void;
}

export interface CloudAgentHostServices {
  readonly workspace: CloudAgentWorkspaceBinding;
  readonly credential: CloudAgentCredentialSource;
  readonly log: CloudAgentLogger;
  acceptArtifact?(candidate: CloudAgentArtifactCandidate, signal?: AbortSignal): Promise<void>;
}

export interface CloudAgentProviderSession extends AsyncDisposable {
  readonly sessionId: string;
  readonly events: AsyncIterable<CloudAgentMessageEnvelope>;
  execute(
    command: CloudAgentCommandEnvelope,
    signal?: AbortSignal,
  ): Promise<CloudAgentMessageEnvelope>;
  close(reason?: string): Promise<void>;
}

export interface CloudAgentProviderPluginV1 {
  readonly abiVersion: typeof CLOUD_AGENT_PROVIDER_PLUGIN_ABI_VERSION;
  readonly providerKind: string;
  describe(signal?: AbortSignal): Promise<CloudAgentProviderDescriptor>;
  createSession(
    input: {
      readonly hostInstanceId: string;
      readonly hostThreadId: string;
      readonly configuration: Readonly<Record<string, unknown>>;
    },
    host: CloudAgentHostServices,
    signal?: AbortSignal,
  ): Promise<CloudAgentProviderSession>;
}
