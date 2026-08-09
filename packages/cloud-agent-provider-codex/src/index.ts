import { chmodSync, mkdirSync, renameSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { join } from "node:path";
import type { CloudAgentProviderPluginV1 } from "@synara/cloud-agent-provider-api";
import {
  createProviderPlugin,
  hasAuthoritativeResumeData,
  nativeResumeContinuationPrompt,
  providerEnvironment,
  providerProcessEnvironment,
  reconstructedPrompt,
  requireProviderOuterSandboxProfile,
  validateRunnerInput,
  type ProviderRunExecutor,
  type ProviderRunOptions,
  type ProviderRunController,
  type RunnerCredential,
  type RunnerInput,
  type RunnerMessage,
} from "@synara/cloud-agent-provider-api/internal";
import { startCodexAppServerRun } from "./codexAppServerRuntime";
import {
  CODEX_NO_TOOL_OPERATION_ENV,
  CODEX_TOOL_POLICY_HOOK_ARGUMENT,
  buildCodexToolPolicyHookCommand,
  buildInlineCodexToolPolicyHookCommand,
  runCodexNoToolAwarePolicyHook,
} from "./codexPostToolUseProvenance";

export {
  CODEX_TOOL_POLICY_HOOK_ARGUMENT,
  buildCodexToolPolicyHookCommand,
  runCodexNoToolAwarePolicyHook,
};
export const CODEX_PROVIDER_KIND = "codex" as const;

type CodexProviderRunOptions = ProviderRunOptions & {
  readonly codexToolPolicyHookCommand?: string;
};

export function startCodexProviderRun(
  input: RunnerInput,
  credential: RunnerCredential | null,
  emit: (message: RunnerMessage) => void,
  options: CodexProviderRunOptions = {},
): ProviderRunController {
  validateRunnerInput(input, { allowEmptyInputText: options.operation !== undefined });
  if (input.workload.provider.trim().toLowerCase() !== "codex")
    throw new Error(`Codex Provider cannot execute provider ${input.workload.provider}.`);
  requireProviderOuterSandboxProfile(options.environment ?? process.env);
  const { environment, redact } = providerEnvironment(
    options.environment ?? process.env,
    credential,
    applyCodexCredentialEnvironment,
  );
  if (credential) {
    const stateRoot = input.providerStateDirectory?.trim() || input.runtimeOutputDirectory?.trim();
    if (!stateRoot)
      throw new Error(
        "Codex Credential requires an agentd-owned providerStateDirectory or runtimeOutputDirectory for isolated CODEX_HOME.",
      );
    environment.CODEX_HOME = writeControlledCodexConfig(stateRoot, environment);
    if (!options.codexToolPolicyHookCommand)
      throw new Error(
        "Codex Credential requires the immutable Provider Host tool-policy hook command.",
      );
  }
  if (options.operation?.commandType === "GenerateText")
    environment[CODEX_NO_TOOL_OPERATION_ENV] = "1";
  const durable = hasAuthoritativeResumeData(input.workload, input.memoryDocuments);
  return startCodexAppServerRun({
    input,
    environment,
    redact,
    emit,
    authoritativePrompt: durable ? reconstructedPrompt(input) : input.workload.inputText,
    nativeResumePrompt: nativeResumeContinuationPrompt(input) ?? input.workload.inputText,
    interactive: options.interactive ?? true,
    ...(options.codexToolPolicyHookCommand
      ? { toolPolicyHookCommand: options.codexToolPolicyHookCommand }
      : {}),
    ...(options.operation ? { operation: options.operation } : {}),
  });
}

export function createCodexProvider(
  options: { readonly toolPolicyHookCommand?: string } = {},
): CloudAgentProviderPluginV1 {
  const toolPolicyHookCommand =
    options.toolPolicyHookCommand ??
    buildInlineCodexToolPolicyHookCommand({
      nodeExecutable: process.execPath,
      electronRunAsNode: process.versions.electron !== undefined,
    });
  const executor: ProviderRunExecutor = (input, credential, emit, runOptions) =>
    startCodexProviderRun(input, credential, emit, {
      ...runOptions,
      codexToolPolicyHookCommand: toolPolicyHookCommand,
    });
  return createProviderPlugin({
    providerKind: CODEX_PROVIDER_KIND,
    displayName: "Codex",
    descriptor: { runtimeVersionProbe: probeCodexVersion },
    configurationSchema: {
      type: "object",
      additionalProperties: false,
      properties: { model: { type: "string", minLength: 1 } },
    },
    startRun: executor,
  });
}

function probeCodexVersion(): { readonly available: boolean; readonly output?: string } {
  const result = spawnSync("codex", ["--version"], {
    encoding: "utf8",
    timeout: 5_000,
    env: providerProcessEnvironment(process.env),
  });
  const output = `${result.stdout ?? ""}\n${result.stderr ?? ""}`.trim();
  return {
    available: result.error === undefined && result.status !== null,
    ...(output ? { output } : {}),
  };
}

function applyCodexCredentialEnvironment(
  environment: NodeJS.ProcessEnv,
  payload: Record<string, unknown>,
): void {
  assertOnlyKeys(payload, ["apiKey", "baseUrl", "organization"]);
  environment.OPENAI_API_KEY = requiredString(payload.apiKey, "Codex Credential apiKey");
  assignOptional(environment, "OPENAI_BASE_URL", payload.baseUrl, "Codex Credential baseUrl");
  assignOptional(
    environment,
    "OPENAI_ORGANIZATION",
    payload.organization,
    "Codex Credential organization",
  );
}
function writeControlledCodexConfig(root: string, environment: NodeJS.ProcessEnv): string {
  const apiKey = requiredString(environment.OPENAI_API_KEY, "Codex Credential apiKey");
  const baseUrl = controlledBaseUrl(environment.OPENAI_BASE_URL);
  const codexHome = join(root, "codex-home");
  mkdirSync(codexHome, { recursive: true, mode: 0o700 });
  chmodSync(codexHome, 0o700);
  const temporaryPath = join(codexHome, "config.toml.tmp");
  const configPath = join(codexHome, "config.toml");
  writeFileSync(
    temporaryPath,
    [
      'model_provider = "synara_controlled"',
      "",
      "[model_providers.synara_controlled]",
      'name = "Synara controlled Credential"',
      `base_url = ${JSON.stringify(baseUrl)}`,
      'env_key = "OPENAI_API_KEY"',
      'wire_api = "responses"',
      "requires_openai_auth = false",
      "",
    ].join("\n"),
    { encoding: "utf8", mode: 0o600 },
  );
  chmodSync(temporaryPath, 0o600);
  renameSync(temporaryPath, configPath);
  chmodSync(configPath, 0o600);
  environment.OPENAI_API_KEY = apiKey;
  return codexHome;
}
function controlledBaseUrl(value: string | undefined): string {
  const candidate = value?.trim() || "https://api.openai.com/v1";
  let parsed: URL;
  try {
    parsed = new URL(candidate);
  } catch {
    throw new Error("Codex Credential baseUrl must be an absolute HTTP(S) URL");
  }
  if (
    !["http:", "https:"].includes(parsed.protocol) ||
    parsed.username ||
    parsed.password ||
    parsed.hash ||
    hasLineBreakOrNull(candidate)
  )
    throw new Error("Codex Credential baseUrl must use HTTP(S) without userinfo or a fragment");
  return candidate.replace(/\/+$/u, "");
}
function assertOnlyKeys(payload: Record<string, unknown>, allowed: ReadonlyArray<string>): void {
  const set = new Set(allowed);
  const extra = Object.keys(payload).find((key) => !set.has(key));
  if (extra) throw new Error(`Codex Credential contains unsupported field ${extra}`);
}
function requiredString(value: unknown, label: string): string {
  const result = optionalString(value, label);
  if (!result) throw new Error(`${label} is required`);
  return result;
}
function optionalString(value: unknown, label: string): string | undefined {
  if (value == null) return undefined;
  if (typeof value !== "string" || !value.trim() || hasLineBreakOrNull(value))
    throw new Error(`${label} must be a non-empty single-line string`);
  return value.trim();
}
function hasLineBreakOrNull(value: string): boolean {
  return value.includes("\r") || value.includes("\n") || value.includes("\0");
}
function assignOptional(
  environment: NodeJS.ProcessEnv,
  key: string,
  value: unknown,
  label: string,
): void {
  const normalized = optionalString(value, label);
  if (normalized) environment[key] = normalized;
}
