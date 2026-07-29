import type { ExternalMcpCapability, RuntimeMode } from "@synara/contracts";

import { GatewayToolError } from "../agentGateway/toolRuntime.ts";

type ExternalMcpRuntimeMode = Exclude<RuntimeMode, "auto">;

export interface ExternalMcpRuntimePolicy {
  readonly environment: "local" | "worktree";
  readonly runtimeMode: ExternalMcpRuntimeMode;
}

export function resolveExternalMcpRuntimePolicy(input: {
  readonly requestedEnvironment?: "local" | "worktree";
  readonly requestedRuntimeMode?: RuntimeMode;
  readonly capabilities: ReadonlySet<ExternalMcpCapability | string>;
}): ExternalMcpRuntimePolicy {
  const environment = input.requestedEnvironment ?? "worktree";
  const runtimeMode = input.requestedRuntimeMode ?? "approval-required";
  if (runtimeMode === "auto") {
    throw new GatewayToolError(
      "capability_denied",
      "Auto execution is available only to Codex and Claude sessions.",
    );
  }
  if (environment === "local") {
    throw new GatewayToolError(
      "capability_denied",
      "External MCP content is untrusted and cannot execute in a local checkout.",
    );
  }
  if (runtimeMode === "full-access") {
    throw new GatewayToolError(
      "capability_denied",
      "External MCP content is untrusted and cannot disable approval-required execution.",
    );
  }
  return { environment: "worktree", runtimeMode: "approval-required" };
}
