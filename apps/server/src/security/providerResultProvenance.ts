import type { ProviderKind } from "@synara/contracts";

export interface ProviderNativeResultProvenanceCapability {
  readonly successfulResult: "host-structural-envelope" | "adjacent-host-context" | "policy-only";
  readonly failedResult: "adjacent-host-context" | "policy-only";
  readonly hostCanRewriteNativeResult: boolean;
  readonly runtimeCoverage: "all-runtimes" | "managed-provider-host-only";
}

/**
 * Exhaustive capability truth, not a security waiver. Providers whose native
 * protocol cannot place Host-authored context before model consumption still
 * rely on the outer sandbox and sensitive-action gate; policy text alone is
 * not accepted as result provenance for server-authored untrusted tasks.
 */
export const PROVIDER_NATIVE_RESULT_PROVENANCE = {
  // Synara disables and attests Codex 0.145's hosted-web, code-mode,
  // browser-use, and computer-use surfaces. The remaining untrusted native
  // results run through exact PreToolUse additionalContext; request_user_input
  // is a live-user exception and tool_search contains only Host-owned metadata.
  // This still cannot rewrite the native result itself.
  codex: {
    successfulResult: "adjacent-host-context",
    failedResult: "adjacent-host-context",
    hostCanRewriteNativeResult: false,
    runtimeCoverage: "all-runtimes",
  },
  claudeAgent: {
    successfulResult: "host-structural-envelope",
    failedResult: "adjacent-host-context",
    hostCanRewriteNativeResult: true,
    runtimeCoverage: "all-runtimes",
  },
  // ACP makes the Agent execute the tool and feed its result back to the model.
  // The Client receives only a later session/update notification, so rewriting
  // that notification would change the transcript projection, not model input.
  cursor: {
    successfulResult: "policy-only",
    failedResult: "policy-only",
    hostCanRewriteNativeResult: false,
    runtimeCoverage: "all-runtimes",
  },
  // Antigravity 2.0 PostToolUse output is fixed to an empty object. Its
  // PreInvocation injection hook is an external same-identity plugin on the
  // local adapter path, not an attested Host boundary.
  antigravity: {
    successfulResult: "policy-only",
    failedResult: "policy-only",
    hostCanRewriteNativeResult: false,
    runtimeCoverage: "all-runtimes",
  },
  grok: {
    successfulResult: "policy-only",
    failedResult: "policy-only",
    hostCanRewriteNativeResult: false,
    runtimeCoverage: "all-runtimes",
  },
  droid: {
    successfulResult: "policy-only",
    failedResult: "policy-only",
    hostCanRewriteNativeResult: false,
    runtimeCoverage: "all-runtimes",
  },
  // OpenCode-compatible tool.execute.after hooks are model-preconsumption, but
  // they are external modules in the Provider process. Synara's mandatory pure
  // mode removes those modules entirely; without a built-in Host hook or exact
  // attestation interface there is no result rewrite boundary to claim.
  kilo: {
    successfulResult: "policy-only",
    failedResult: "policy-only",
    hostCanRewriteNativeResult: false,
    runtimeCoverage: "all-runtimes",
  },
  opencode: {
    successfulResult: "policy-only",
    failedResult: "policy-only",
    hostCanRewriteNativeResult: false,
    runtimeCoverage: "all-runtimes",
  },
  pi: {
    successfulResult: "adjacent-host-context",
    failedResult: "adjacent-host-context",
    hostCanRewriteNativeResult: true,
    runtimeCoverage: "all-runtimes",
  },
} as const satisfies Record<ProviderKind, ProviderNativeResultProvenanceCapability>;

export type ProviderResultProvenanceRuntimeBoundary = "local-adapter" | "managed-provider-host";

export function providerHasModelPreconsumptionResultProvenance(
  provider: ProviderKind,
  runtimeBoundary: ProviderResultProvenanceRuntimeBoundary = "local-adapter",
): boolean {
  const capability: ProviderNativeResultProvenanceCapability =
    PROVIDER_NATIVE_RESULT_PROVENANCE[provider];
  const coversRuntime =
    capability.runtimeCoverage === "all-runtimes" || runtimeBoundary === "managed-provider-host";
  return (
    coversRuntime &&
    capability.successfulResult !== "policy-only" &&
    capability.failedResult !== "policy-only"
  );
}
