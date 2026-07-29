// FILE: codexRuntimeIsolation.ts
// Purpose: Shared Codex startup isolation and exact Host tool-policy hook configuration.
// Layer: Shared runtime policy

export const CODEX_DISABLED_EXECUTABLE_CONFIG_FEATURES = [
  "apps",
  "enable_mcp_apps",
  "executor_capability_discovery",
  "external_agent_memory_import",
  "hooks",
  "in_app_browser",
  "plugin_sharing",
  "plugins",
  "remote_plugin",
  "shell_snapshot",
  "skill_mcp_dependency_install",
  // Codex 0.145 intentionally skips PreToolUse for write_stdin continuations.
  // Use one-shot shell_command so every model-authored command is classified.
  "unified_exec",
] as const;

/**
 * Codex 0.145 routes ordinary function/custom tools through ToolRegistry, where
 * the Host PreToolUse hook runs. These surfaces either execute outside that
 * registry or can aggregate nested results without an outer hook, so keep them
 * unavailable until Codex exposes an equivalent attestation boundary.
 */
export const CODEX_DISABLED_UNATTESTED_TOOL_FEATURES = [
  "browser_use",
  "browser_use_external",
  "browser_use_full_cdp_access",
  "code_mode",
  "code_mode_buffered_exec",
  "code_mode_host",
  "code_mode_only",
  "computer_use",
  "standalone_web_search",
  "web_search_cached",
  "web_search_request",
] as const;

export const CODEX_DISABLED_RUNTIME_FEATURES = [
  ...CODEX_DISABLED_EXECUTABLE_CONFIG_FEATURES,
  ...CODEX_DISABLED_UNATTESTED_TOOL_FEATURES,
] as const;

// Cached hosted search is Codex 0.145's default even when both legacy feature
// flags are false. Its result is consumed inside the hosted Responses tool and
// never reaches ToolRegistry/PreToolUse, so the top-level mode must be pinned.
export const CODEX_HOSTED_TOOL_ISOLATION_CONFIG = ['web_search="disabled"'] as const;

export const CODEX_TOOL_POLICY_HOOK_CONTEXT_LIMIT = 512;
export const CODEX_TOOL_POLICY_HOOK_TIMEOUT_SECONDS = 5;
export const CODEX_TOOL_POLICY_HOOK_COMMAND_LIMIT = 4_096;
export const CODEX_TOOL_POLICY_THREAD_CONFIG = {
  // Codex 0.145 app-server does not propagate the CLI bypass flag into a
  // Thread's Hook Engine. This request-level override is authoritative.
  bypass_hook_trust: true,
} as const;

export interface CodexRuntimeIsolationExpectedMcpServer {
  readonly name: string;
  readonly url: string;
  readonly bearerTokenEnvVar: string;
}

const CODEX_ATTESTED_HTTP_MCP_SERVER_KEYS = [
  "bearer_token_env_var",
  "enabled",
  "environment_id",
  "tool_timeout_sec",
  "url",
] as const;

export function codexExecutableConfigIsolationArguments(
  mcpServersOverride = "mcp_servers={}",
): ReadonlyArray<string> {
  return [
    "app-server",
    "--strict-config",
    "--config",
    mcpServersOverride,
    ...CODEX_HOSTED_TOOL_ISOLATION_CONFIG.flatMap((config) => ["--config", config]),
    ...CODEX_DISABLED_RUNTIME_FEATURES.flatMap((feature) => [
      "--config",
      `features.${feature}=false`,
    ]),
  ];
}

export function codexAppServerArgumentsWithToolPolicyHook(
  baseArguments: ReadonlyArray<string>,
  hookCommand: string,
): ReadonlyArray<string> {
  if (baseArguments[0] !== "app-server") {
    throw new Error("Codex tool-policy hook arguments require an app-server base command.");
  }
  if (
    hookCommand.length === 0 ||
    hookCommand.length > CODEX_TOOL_POLICY_HOOK_COMMAND_LIMIT ||
    /[\r\n\0]/u.test(hookCommand)
  ) {
    throw new Error("Codex tool-policy hook command is invalid.");
  }
  const hookConfig = [
    '{matcher=".*",hooks=[{type="command"',
    `command=${JSON.stringify(hookCommand)}`,
    `timeout=${CODEX_TOOL_POLICY_HOOK_TIMEOUT_SECONDS}`,
    `additionalContextLimit=${CODEX_TOOL_POLICY_HOOK_CONTEXT_LIMIT}}]}]`,
  ].join(",");
  return [
    "--dangerously-bypass-hook-trust",
    ...baseArguments,
    "--config",
    "features.hooks=true",
    "--config",
    `hooks.PreToolUse=[${hookConfig}`,
  ];
}

export function isCodexToolPolicyHookAttested(response: unknown, expectedCommand: string): boolean {
  const result = asRecord(response);
  const workspaces = Array.isArray(result?.data) ? result.data : [];
  const hooks = workspaces.flatMap((workspace) => {
    const record = asRecord(workspace);
    return Array.isArray(record?.hooks)
      ? record.hooks.flatMap((hook) => {
          const parsed = asRecord(hook);
          return parsed ? [parsed] : [];
        })
      : [];
  });
  const enabledNonManagedHooks = hooks.filter(
    (hook) => hook.enabled === true && hook.isManaged !== true,
  );
  const matchesEvent = (hook: Record<string, unknown>, eventName: string): boolean =>
    hook.eventName === eventName &&
    hook.handlerType === "command" &&
    hook.matcher === ".*" &&
    hook.command === expectedCommand &&
    hook.timeoutSec === CODEX_TOOL_POLICY_HOOK_TIMEOUT_SECONDS &&
    hook.additionalContextLimit === CODEX_TOOL_POLICY_HOOK_CONTEXT_LIMIT &&
    hook.source === "sessionFlags" &&
    hook.trustStatus === "untrusted";
  const matchingHooks = enabledNonManagedHooks.filter((hook) => matchesEvent(hook, "preToolUse"));
  const hasDiagnostics = workspaces.some((workspace) => {
    const record = asRecord(workspace);
    return [record?.warnings, record?.errors].some(
      (diagnostics) => Array.isArray(diagnostics) && diagnostics.length > 0,
    );
  });
  return (
    !hasDiagnostics &&
    matchingHooks.length === 1 &&
    enabledNonManagedHooks.length === matchingHooks.length
  );
}

/**
 * Attest the effective app-server configuration, not only the CLI arguments
 * Synara requested. This catches ignored/overridden feature switches before a
 * thread can expose a hosted or code-mode tool outside the Host hook boundary.
 */
export function isCodexRuntimeIsolationConfigAttested(
  response: unknown,
  expectedMcpServers: ReadonlyArray<CodexRuntimeIsolationExpectedMcpServer>,
  expectedShellExcludedEnvVarNames: ReadonlyArray<string> = expectedMcpServers.map(
    (server) => server.bearerTokenEnvVar,
  ),
): boolean {
  const config = asRecord(asRecord(response)?.config);
  const features = asRecord(config?.features);
  const mcpServers = asRecord(config?.mcp_servers);
  if (config?.web_search !== "disabled" || features === undefined || mcpServers === undefined) {
    return false;
  }
  for (const feature of CODEX_DISABLED_RUNTIME_FEATURES) {
    const expected = feature === "hooks";
    if (features[feature] !== expected) return false;
  }
  const actualMcpServerNames = Object.keys(mcpServers).sort();
  const expectedNames = expectedMcpServers.map((server) => server.name).sort();
  if (
    new Set(expectedNames).size !== expectedNames.length ||
    actualMcpServerNames.length !== expectedNames.length ||
    !actualMcpServerNames.every((name, index) => name === expectedNames[index])
  ) {
    return false;
  }

  const shellEnvironmentPolicy = asRecord(config?.shell_environment_policy);
  if (
    !expectedMcpServers.every((expected) => {
      const actual = asRecord(mcpServers[expected.name]);
      return (
        isAttestedLoopbackMcpUrl(expected.url) &&
        /^[A-Za-z_][A-Za-z0-9_]*$/u.test(expected.bearerTokenEnvVar) &&
        hasExactKeys(actual, CODEX_ATTESTED_HTTP_MCP_SERVER_KEYS) &&
        actual?.url === expected.url &&
        actual?.bearer_token_env_var === expected.bearerTokenEnvVar &&
        actual?.environment_id === "local" &&
        actual?.enabled === true &&
        actual?.tool_timeout_sec === null
      );
    })
  ) {
    return false;
  }

  const requiredExclusions = [
    ...new Set([
      ...expectedShellExcludedEnvVarNames,
      ...expectedMcpServers.map((server) => server.bearerTokenEnvVar),
    ]),
  ];
  return isShellEnvironmentPolicyAttested(shellEnvironmentPolicy, requiredExclusions);
}

function isAttestedLoopbackMcpUrl(value: string): boolean {
  try {
    const parsed = new URL(value);
    return (
      parsed.protocol === "http:" &&
      (parsed.hostname === "127.0.0.1" ||
        parsed.hostname === "[::1]" ||
        parsed.hostname === "::1") &&
      parsed.port.length > 0 &&
      parsed.pathname === "/mcp" &&
      parsed.username.length === 0 &&
      parsed.password.length === 0 &&
      parsed.search.length === 0 &&
      parsed.hash.length === 0
    );
  } catch {
    return false;
  }
}

function isShellEnvironmentPolicyAttested(
  policy: Record<string, unknown> | undefined,
  expectedExcludedEnvVarNames: ReadonlyArray<string>,
): boolean {
  const normalizedExpectedNames = expectedExcludedEnvVarNames.map((name) => name.toUpperCase());
  if (
    new Set(normalizedExpectedNames).size !== normalizedExpectedNames.length ||
    expectedExcludedEnvVarNames.some((name) => !/^[A-Za-z_][A-Za-z0-9_]*$/u.test(name))
  ) {
    return false;
  }
  if (!policy) return normalizedExpectedNames.length === 0;
  if (
    (policy.inherit !== null && policy.inherit !== undefined) ||
    (policy.ignore_default_excludes !== null &&
      policy.ignore_default_excludes !== undefined &&
      policy.ignore_default_excludes !== false) ||
    (policy.set !== null && policy.set !== undefined) ||
    (policy.include_only !== null && policy.include_only !== undefined) ||
    (policy.experimental_use_profile !== null && policy.experimental_use_profile !== undefined)
  ) {
    return false;
  }

  if (policy.exclude === null || policy.exclude === undefined) {
    return normalizedExpectedNames.length === 0;
  }
  if (
    !Array.isArray(policy.exclude) ||
    !policy.exclude.every((value): value is string => typeof value === "string")
  ) {
    return false;
  }
  const normalizedActualExclusions = new Set(policy.exclude.map((name) => name.toUpperCase()));
  return normalizedExpectedNames.every((name) => normalizedActualExclusions.has(name));
}

function hasExactKeys(
  value: Record<string, unknown> | undefined,
  expectedKeys: ReadonlyArray<string>,
): boolean {
  if (!value) return false;
  const actualKeys = Object.keys(value).sort();
  const sortedExpectedKeys = [...expectedKeys].sort();
  return (
    actualKeys.length === sortedExpectedKeys.length &&
    actualKeys.every((key, index) => key === sortedExpectedKeys[index])
  );
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}
