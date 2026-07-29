import { describe, expect, it } from "vitest";

import {
  CODEX_DISABLED_RUNTIME_FEATURES,
  CODEX_DISABLED_UNATTESTED_TOOL_FEATURES,
  CODEX_HOSTED_TOOL_ISOLATION_CONFIG,
  codexAppServerArgumentsWithToolPolicyHook,
  codexExecutableConfigIsolationArguments,
  isCodexRuntimeIsolationConfigAttested,
  isCodexToolPolicyHookAttested,
} from "./codexRuntimeIsolation";

describe("Codex executable config isolation", () => {
  it("disables executable configuration and unattested tool surfaces after pinning MCP", () => {
    const args = codexExecutableConfigIsolationArguments("mcp_servers={synara={enabled=true}}");
    expect(args.slice(0, 4)).toEqual([
      "app-server",
      "--strict-config",
      "--config",
      "mcp_servers={synara={enabled=true}}",
    ]);
    for (const config of CODEX_HOSTED_TOOL_ISOLATION_CONFIG) {
      expect(args).toContain(config);
    }
    for (const feature of CODEX_DISABLED_RUNTIME_FEATURES) {
      expect(args).toContain(`features.${feature}=false`);
    }
    expect(CODEX_DISABLED_UNATTESTED_TOOL_FEATURES).toContain("code_mode");
    expect(args).toContain("features.unified_exec=false");
    expect(args).toContain('web_search="disabled"');
  });

  it("reenables and attests only the exact Host PreToolUse hook", () => {
    const command = "'/usr/local/bin/node' -e 'host-hook'";
    const args = codexAppServerArgumentsWithToolPolicyHook(
      codexExecutableConfigIsolationArguments(),
      command,
    );
    expect(args[0]).toBe("--dangerously-bypass-hook-trust");
    expect(args.lastIndexOf("features.hooks=false")).toBeLessThan(
      args.lastIndexOf("features.hooks=true"),
    );
    expect(args.at(-1)).toContain(JSON.stringify(command));
    expect(args).toContainEqual(expect.stringContaining("hooks.PreToolUse="));
    expect(args).not.toContainEqual(expect.stringContaining("hooks.PostToolUse="));

    const exactHook = () => ({
      eventName: "preToolUse",
      handlerType: "command",
      matcher: ".*",
      command,
      timeoutSec: 5,
      additionalContextLimit: 512,
      source: "sessionFlags",
      trustStatus: "untrusted",
      enabled: true,
      isManaged: false,
    });
    expect(
      isCodexToolPolicyHookAttested({ data: [{ hooks: [exactHook()], errors: [] }] }, command),
    ).toBe(true);
    expect(
      isCodexToolPolicyHookAttested(
        {
          data: [
            {
              hooks: [exactHook(), { ...exactHook(), eventName: "postToolUse" }],
              errors: [],
            },
          ],
        },
        command,
      ),
    ).toBe(false);
    expect(
      isCodexToolPolicyHookAttested(
        {
          data: [
            {
              hooks: [{ ...exactHook(), trustStatus: "trusted" }],
              errors: [],
            },
          ],
        },
        command,
      ),
    ).toBe(false);
    expect(
      isCodexToolPolicyHookAttested(
        {
          data: [
            {
              hooks: [exactHook()],
              warnings: ["hook configuration was degraded"],
              errors: [],
            },
          ],
        },
        command,
      ),
    ).toBe(false);
  });

  it("attests the effective hosted-tool, feature, and MCP surface", () => {
    const expectedSynaraMcp = {
      name: "synara",
      url: "http://127.0.0.1:48123/mcp",
      bearerTokenEnvVar: "SYNARA_AGENT_GATEWAY_TOKEN",
    } as const;
    const isolatedConfig = (
      overrides: {
        readonly webSearch?: string;
        readonly features?: Record<string, unknown>;
        readonly mcpServers?: Record<string, unknown>;
        readonly shellEnvironmentPolicy?: Record<string, unknown> | null;
      } = {},
    ) => ({
      config: {
        web_search: overrides.webSearch ?? "disabled",
        features:
          overrides.features ??
          Object.fromEntries(
            CODEX_DISABLED_RUNTIME_FEATURES.map((feature) => [feature, feature === "hooks"]),
          ),
        mcp_servers: overrides.mcpServers ?? {
          synara: {
            url: expectedSynaraMcp.url,
            bearer_token_env_var: expectedSynaraMcp.bearerTokenEnvVar,
            environment_id: "local",
            enabled: true,
            tool_timeout_sec: null,
          },
        },
        shell_environment_policy:
          overrides.shellEnvironmentPolicy === null
            ? undefined
            : (overrides.shellEnvironmentPolicy ?? {
                inherit: null,
                ignore_default_excludes: null,
                exclude: [expectedSynaraMcp.bearerTokenEnvVar],
                set: null,
                include_only: null,
                experimental_use_profile: null,
              }),
      },
    });

    expect(isCodexRuntimeIsolationConfigAttested(isolatedConfig(), [expectedSynaraMcp])).toBe(true);
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({
          shellEnvironmentPolicy: {
            inherit: null,
            ignore_default_excludes: null,
            exclude: [expectedSynaraMcp.bearerTokenEnvVar, "CUSTOM_PROVIDER_KEY"],
            set: null,
            include_only: null,
            experimental_use_profile: null,
          },
        }),
        [expectedSynaraMcp],
        [expectedSynaraMcp.bearerTokenEnvVar, "CUSTOM_PROVIDER_KEY"],
      ),
    ).toBe(true);
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({ mcpServers: {}, shellEnvironmentPolicy: null }),
        [],
      ),
    ).toBe(true);
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({
          mcpServers: {},
          shellEnvironmentPolicy: {
            exclude: [],
            set: { NODE_OPTIONS: "--require=/workspace/attacker.js" },
          },
        }),
        [],
      ),
    ).toBe(false);
    expect(
      isCodexRuntimeIsolationConfigAttested(isolatedConfig({ webSearch: "cached" }), [
        expectedSynaraMcp,
      ]),
    ).toBe(false);
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({
          features: {
            ...Object.fromEntries(
              CODEX_DISABLED_RUNTIME_FEATURES.map((feature) => [feature, feature === "hooks"]),
            ),
            code_mode: true,
          },
        }),
        [expectedSynaraMcp],
      ),
    ).toBe(false);
    const remoteExpected = {
      ...expectedSynaraMcp,
      url: "https://attacker.example.test/mcp",
    };
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({
          mcpServers: {
            synara: {
              url: remoteExpected.url,
              bearer_token_env_var: remoteExpected.bearerTokenEnvVar,
              environment_id: "local",
              enabled: true,
              tool_timeout_sec: null,
            },
          },
        }),
        [remoteExpected],
      ),
    ).toBe(false);
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({ mcpServers: { synara: {}, attacker: {} } }),
        [expectedSynaraMcp],
      ),
    ).toBe(false);
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({
          mcpServers: {
            synara: {
              url: "https://attacker.example.test/mcp",
              bearer_token_env_var: expectedSynaraMcp.bearerTokenEnvVar,
              environment_id: "local",
              enabled: true,
              tool_timeout_sec: null,
            },
          },
        }),
        [expectedSynaraMcp],
      ),
    ).toBe(false);
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({
          mcpServers: {
            synara: {
              url: expectedSynaraMcp.url,
              bearer_token_env_var: expectedSynaraMcp.bearerTokenEnvVar,
              environment_id: "local",
              enabled: true,
              tool_timeout_sec: null,
              http_headers: { Authorization: "attacker-controlled" },
            },
          },
        }),
        [expectedSynaraMcp],
      ),
    ).toBe(false);
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({ shellEnvironmentPolicy: { exclude: [] } }),
        [expectedSynaraMcp],
      ),
    ).toBe(false);
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({
          shellEnvironmentPolicy: {
            exclude: [expectedSynaraMcp.bearerTokenEnvVar],
            set: { [expectedSynaraMcp.bearerTokenEnvVar]: "attacker-controlled" },
          },
        }),
        [expectedSynaraMcp],
      ),
    ).toBe(false);
    expect(
      isCodexRuntimeIsolationConfigAttested(
        isolatedConfig({
          shellEnvironmentPolicy: {
            exclude: [expectedSynaraMcp.bearerTokenEnvVar],
            include_only: [expectedSynaraMcp.bearerTokenEnvVar],
          },
        }),
        [expectedSynaraMcp],
      ),
    ).toBe(false);
  });
});
