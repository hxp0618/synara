export type CloudAgentPackageName =
  | "@synara/cloud-agent-distribution"
  | "@synara/cloud-agent-protocol"
  | "@synara/cloud-agent-provider-api"
  | "@synara/cloud-agent-provider-claude"
  | "@synara/cloud-agent-provider-codex"
  | "@synara/cloud-agent-runtime"
  | "@synara/cloud-agent-testkit";

export type CloudAgentPackageArtifact = {
  name: CloudAgentPackageName;
  version: string;
  url: string;
  sha256: `sha256:${string}`;
};

export type ValidatedCloudAgentCandidate = {
  repository: string;
  tag: string;
  sourceCommit: string;
  candidateDigest: `sha256:${string}`;
  standaloneRuntime: { url: string; sha256: `sha256:${string}` };
  packages: CloudAgentPackageArtifact[];
  packageVersions: Record<CloudAgentPackageName, string>;
};

export const CLOUD_AGENT_RELEASE_REPOSITORY: string;
export const CLOUD_AGENT_RELEASE_TAG: string;
export const CLOUD_AGENT_RELEASE_BASE_URL: string;
export const CLOUD_AGENT_SOURCE_COMMIT: string;
export const CLOUD_AGENT_STANDALONE_RUNTIME: Readonly<{
  filename: string;
  sha256: `sha256:${string}`;
}>;
export const CLOUD_AGENT_PACKAGE_SPECS: readonly Readonly<
  CloudAgentPackageArtifact & { filename: string }
>[];
export const CLOUD_AGENT_PACKAGE_NAMES: readonly CloudAgentPackageName[];

export function canonicalCloudAgentCandidateDigest(
  packages: ReadonlyArray<Pick<CloudAgentPackageArtifact, "name" | "version" | "sha256">>,
): `sha256:${string}`;

export function validateCloudAgentCandidateLock(value: unknown): ValidatedCloudAgentCandidate;
