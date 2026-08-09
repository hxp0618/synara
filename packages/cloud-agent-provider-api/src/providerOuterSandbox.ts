export const PROVIDER_OUTER_SANDBOX_PROFILE_ENV = "SYNARA_PROVIDER_OUTER_SANDBOX_PROFILE";

export const PROVIDER_OUTER_SANDBOX_PROFILES = [
  "kubernetes-restricted-v1",
  "gvisor-sandboxed-v1",
  "microvm-isolated-v1",
  "single-tenant-trusted-v1",
] as const;

export type ProviderOuterSandboxProfile = (typeof PROVIDER_OUTER_SANDBOX_PROFILES)[number];

export function requireProviderOuterSandboxProfile(
  environment: NodeJS.ProcessEnv,
): ProviderOuterSandboxProfile {
  const value = environment[PROVIDER_OUTER_SANDBOX_PROFILE_ENV]?.trim();
  if (PROVIDER_OUTER_SANDBOX_PROFILES.includes(value as ProviderOuterSandboxProfile)) {
    return value as ProviderOuterSandboxProfile;
  }
  throw new Error(
    "Provider execution refused: the host did not supply an allowed outer sandbox or explicit local trust profile.",
  );
}
