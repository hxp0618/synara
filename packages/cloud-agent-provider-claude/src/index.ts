import type { CloudAgentProviderPluginV1 } from "@synara/cloud-agent-provider-api";
import {
  createProviderPlugin,
  hasAuthoritativeResumeData,
  nativeResumeContinuationPrompt,
  providerEnvironment,
  reconstructedPrompt,
  requireProviderOuterSandboxProfile,
  validateRunnerInput,
  type ProviderRunExecutor,
  type ProviderRunOptions,
  type RunnerCredential,
  type RunnerInput,
  type RunnerMessage,
  type ProviderRunController,
} from "@synara/cloud-agent-provider-api/internal";

import { startClaudeAgentSdkRun, type ClaudeQueryFactory } from "./claudeAgentSdkRuntime";

export const CLAUDE_PROVIDER_KIND = "claudeAgent" as const;
const CLAUDE_AGENT_SDK_VERSION = "0.3.207";

type ClaudeProviderRunOptions = ProviderRunOptions & {
  readonly claudeQueryFactory?: ClaudeQueryFactory;
};

export function startClaudeProviderRun(
  input: RunnerInput,
  credential: RunnerCredential | null,
  emit: (message: RunnerMessage) => void,
  options: ClaudeProviderRunOptions = {},
): ProviderRunController {
  validateRunnerInput(input, { allowEmptyInputText: options.operation !== undefined });
  assertClaudeProvider(input.workload.provider);
  requireProviderOuterSandboxProfile(options.environment ?? process.env);
  const { environment, redact } = providerEnvironment(
    options.environment ?? process.env,
    credential,
    applyClaudeCredentialEnvironment,
  );
  const hasDurableHistory = hasAuthoritativeResumeData(input.workload, input.memoryDocuments);
  return startClaudeAgentSdkRun({
    input,
    environment,
    usesAmbientAuthentication: credential === null,
    redact,
    emit,
    authoritativePrompt: hasDurableHistory ? reconstructedPrompt(input) : input.workload.inputText,
    nativeResumePrompt: nativeResumeContinuationPrompt(input) ?? input.workload.inputText,
    interactive: options.interactive ?? true,
    ...(options.operation ? { operation: options.operation } : {}),
    ...(options.claudeQueryFactory ? { queryFactory: options.claudeQueryFactory } : {}),
  });
}

const claudeExecutor: ProviderRunExecutor = (input, credential, emit, options) =>
  startClaudeProviderRun(input, credential, emit, options);

export function createClaudeProvider(): CloudAgentProviderPluginV1 {
  return createProviderPlugin({
    providerKind: CLAUDE_PROVIDER_KIND,
    displayName: "Claude",
    providerAliases: ["claude"],
    descriptor: { runtimeVersion: CLAUDE_AGENT_SDK_VERSION },
    configurationSchema: {
      type: "object",
      additionalProperties: false,
      properties: { model: { type: "string", minLength: 1 } },
    },
    startRun: claudeExecutor,
  });
}

function assertClaudeProvider(value: string): void {
  const normalized = value.trim().toLowerCase();
  if (normalized !== "claude" && normalized !== "claudeagent") {
    throw new Error(`Claude Provider cannot execute provider ${value}.`);
  }
}

function applyClaudeCredentialEnvironment(
  environment: NodeJS.ProcessEnv,
  payload: Record<string, unknown>,
): void {
  assertOnlyKeys(payload, ["apiKey", "authToken", "baseUrl"]);
  const apiKey = optionalString(payload.apiKey, "Claude Credential apiKey");
  const authToken = optionalString(payload.authToken, "Claude Credential authToken");
  if ((apiKey ? 1 : 0) + (authToken ? 1 : 0) !== 1) {
    throw new Error("Claude Credential requires exactly one of apiKey or authToken");
  }
  if (apiKey) environment.ANTHROPIC_API_KEY = apiKey;
  if (authToken) environment.ANTHROPIC_AUTH_TOKEN = authToken;
  const baseUrl = optionalString(payload.baseUrl, "Claude Credential baseUrl");
  if (baseUrl) environment.ANTHROPIC_BASE_URL = baseUrl;
}

function assertOnlyKeys(payload: Record<string, unknown>, allowed: ReadonlyArray<string>): void {
  const allowedKeys = new Set(allowed);
  const extra = Object.keys(payload).find((key) => !allowedKeys.has(key));
  if (extra) throw new Error(`Claude Credential contains unsupported field ${extra}`);
}

function optionalString(value: unknown, label: string): string | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== "string" || !value.trim() || /[\r\n\0]/u.test(value)) {
    throw new Error(`${label} must be a non-empty single-line string`);
  }
  return value.trim();
}
