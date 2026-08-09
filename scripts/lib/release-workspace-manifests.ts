// FILE: release-workspace-manifests.ts
// Purpose: Single source for workspace importers copied into release verification/staging roots.
// Layer: Release/build helper

export const RELEASE_WORKSPACE_MANIFEST_PATHS = [
  "package.json",
  "apps/admin/package.json",
  "apps/server/package.json",
  "apps/desktop/package.json",
  "apps/developer-docs/package.json",
  "apps/web/package.json",
  "apps/marketing/package.json",
  "apps/provider-host/package.json",
  "packages/cloud-agent-distribution/package.json",
  "packages/cloud-agent-protocol/package.json",
  "packages/cloud-agent-provider-api/package.json",
  "packages/cloud-agent-provider-claude/package.json",
  "packages/cloud-agent-provider-codex/package.json",
  "packages/cloud-agent-runtime/package.json",
  "packages/cloud-agent-testkit/package.json",
  "packages/contracts/package.json",
  "packages/control-plane-client/package.json",
  "packages/enterprise-ui/package.json",
  "packages/polaris-sdk/package.json",
  "packages/shared/package.json",
  "scripts/package.json",
] as const;

export const RELEASE_LOCKFILE_PATH = "bun.lock";
export const RELEASE_PATCHES_PATH = "patches";
