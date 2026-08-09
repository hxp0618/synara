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
  "unified_exec",
] as const;
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
export const CODEX_HOSTED_TOOL_ISOLATION_CONFIG = ['web_search="disabled"'] as const;
export const CODEX_TOOL_POLICY_HOOK_CONTEXT_LIMIT = 512;
export const CODEX_TOOL_POLICY_HOOK_TIMEOUT_SECONDS = 5;
export const CODEX_TOOL_POLICY_HOOK_COMMAND_LIMIT = 4_096;
export const CODEX_TOOL_POLICY_THREAD_CONFIG = { bypass_hook_trust: true } as const;
export interface CodexRuntimeIsolationExpectedMcpServer {
  readonly name: string;
  readonly url: string;
  readonly bearerTokenEnvVar: string;
}

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
  if (baseArguments[0] !== "app-server")
    throw new Error("Codex tool-policy hook arguments require an app-server base command.");
  if (
    !hookCommand ||
    hookCommand.length > CODEX_TOOL_POLICY_HOOK_COMMAND_LIMIT ||
    /[\r\n\0]/u.test(hookCommand)
  )
    throw new Error("Codex tool-policy hook command is invalid.");
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
  const hooks = workspaces.flatMap((workspace) =>
    Array.isArray(asRecord(workspace)?.hooks)
      ? (asRecord(workspace)?.hooks as unknown[]).flatMap((hook) =>
          asRecord(hook) ? [asRecord(hook)!] : [],
        )
      : [],
  );
  const enabled = hooks.filter((hook) => hook.enabled === true && hook.isManaged !== true);
  const matches = enabled.filter(
    (hook) =>
      hook.eventName === "preToolUse" &&
      hook.handlerType === "command" &&
      hook.matcher === ".*" &&
      hook.command === expectedCommand &&
      hook.timeoutSec === 5 &&
      hook.additionalContextLimit === 512 &&
      hook.source === "sessionFlags" &&
      hook.trustStatus === "untrusted",
  );
  const diagnostics = workspaces.some((workspace) =>
    [asRecord(workspace)?.warnings, asRecord(workspace)?.errors].some(
      (value) => Array.isArray(value) && value.length > 0,
    ),
  );
  return !diagnostics && matches.length === 1 && enabled.length === 1;
}
export function isCodexRuntimeIsolationConfigAttested(
  response: unknown,
  expectedMcpServers: ReadonlyArray<CodexRuntimeIsolationExpectedMcpServer>,
  expectedExcluded: ReadonlyArray<string> = expectedMcpServers.map(
    (server) => server.bearerTokenEnvVar,
  ),
): boolean {
  const config = asRecord(asRecord(response)?.config);
  const features = asRecord(config?.features);
  const servers = asRecord(config?.mcp_servers);
  if (config?.web_search !== "disabled" || !features || !servers) return false;
  for (const feature of CODEX_DISABLED_RUNTIME_FEATURES)
    if (features[feature] !== (feature === "hooks")) return false;
  const names = Object.keys(servers).sort();
  const expectedNames = expectedMcpServers.map((server) => server.name).sort();
  if (
    names.length !== expectedNames.length ||
    names.some((name, index) => name !== expectedNames[index])
  )
    return false;
  for (const expected of expectedMcpServers) {
    const actual = asRecord(servers[expected.name]);
    if (
      !actual ||
      actual.url !== expected.url ||
      actual.bearer_token_env_var !== expected.bearerTokenEnvVar ||
      actual.environment_id !== "local" ||
      actual.enabled !== true ||
      actual.tool_timeout_sec !== null ||
      !isLoopbackMcpUrl(expected.url)
    )
      return false;
  }
  const policy = asRecord(config?.shell_environment_policy);
  if (!policy) return expectedExcluded.length === 0;
  if (
    policy.inherit != null ||
    policy.set != null ||
    policy.include_only != null ||
    policy.experimental_use_profile != null ||
    (policy.ignore_default_excludes != null && policy.ignore_default_excludes !== false) ||
    !Array.isArray(policy.exclude)
  )
    return false;
  const actualExclusions = new Set(
    (policy.exclude as unknown[])
      .filter((value): value is string => typeof value === "string")
      .map((value) => value.toUpperCase()),
  );
  return expectedExcluded.every((name) => actualExclusions.has(name.toUpperCase()));
}
function isLoopbackMcpUrl(value: string): boolean {
  try {
    const url = new URL(value);
    return (
      url.protocol === "http:" &&
      ["127.0.0.1", "[::1]", "::1"].includes(url.hostname) &&
      !!url.port &&
      url.pathname === "/mcp" &&
      !url.username &&
      !url.password &&
      !url.search &&
      !url.hash
    );
  } catch {
    return false;
  }
}
function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}
