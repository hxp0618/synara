// FILE: release-smoke.ts
// Purpose: Smoke-tests release version alignment and merged macOS updater manifests.
// Layer: Release verification script
// Depends on: update-release-package-versions.ts and merge-mac-update-manifests.ts.

import { execFileSync } from "node:child_process";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  SYNARA_DESKTOP_UPDATE_CHANNEL,
  SYNARA_PRODUCTION_BUNDLE_ID,
} from "@synara/shared/desktopIdentity";

import {
  readReleaseUpdatePolicyConfig,
  resolveReleaseUpdatePolicy,
} from "./lib/release-update-policy.ts";
import {
  RELEASE_LOCKFILE_PATH,
  RELEASE_PATCHES_PATH,
  RELEASE_WORKSPACE_MANIFEST_PATHS,
} from "./lib/release-workspace-manifests.ts";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");

function copyWorkspaceManifestFixture(targetRoot: string): void {
  for (const relativePath of RELEASE_WORKSPACE_MANIFEST_PATHS) {
    const sourcePath = resolve(repoRoot, relativePath);
    const destinationPath = resolve(targetRoot, relativePath);
    mkdirSync(dirname(destinationPath), { recursive: true });
    cpSync(sourcePath, destinationPath);
  }
  cpSync(resolve(repoRoot, RELEASE_LOCKFILE_PATH), resolve(targetRoot, RELEASE_LOCKFILE_PATH));
  cpSync(resolve(repoRoot, RELEASE_PATCHES_PATH), resolve(targetRoot, RELEASE_PATCHES_PATH), {
    recursive: true,
  });
}

function writeMacManifestFixtures(targetRoot: string): { arm64Path: string; x64Path: string } {
  const assetDirectory = resolve(targetRoot, "release-assets");
  mkdirSync(assetDirectory, { recursive: true });

  const arm64Path = resolve(assetDirectory, "latest-mac.yml");
  const x64Path = resolve(assetDirectory, "latest-mac-x64.yml");

  writeFileSync(
    arm64Path,
    `version: 9.9.9-smoke.0
files:
  - url: Synara-9.9.9-smoke.0-arm64.zip
    sha512: arm64zip
    size: 125621344
path: Synara-9.9.9-smoke.0-arm64.zip
sha512: arm64zip
releaseDate: '2026-03-08T10:32:14.587Z'
`,
  );

  writeFileSync(
    x64Path,
    `version: 9.9.9-smoke.0
files:
  - url: Synara-9.9.9-smoke.0-x64.zip
    sha512: x64zip
    size: 132000112
path: Synara-9.9.9-smoke.0-x64.zip
sha512: x64zip
releaseDate: '2026-03-08T10:36:07.540Z'
`,
  );

  return { arm64Path, x64Path };
}

function assertContains(haystack: string, needle: string, message: string): void {
  if (!haystack.includes(needle)) {
    throw new Error(message);
  }
}

function assertNotContains(haystack: string, needle: string, message: string): void {
  if (haystack.includes(needle)) {
    throw new Error(message);
  }
}

function verifyCanonicalIdentity(): void {
  const serverPackage = JSON.parse(
    readFileSync(resolve(repoRoot, "apps/server/package.json"), "utf8"),
  ) as { name?: string; bin?: Record<string, string> };
  if (serverPackage.name !== "@synara/cli") {
    throw new Error(`Expected CLI package @synara/cli, got ${serverPackage.name ?? "<missing>"}.`);
  }
  const expectedBinaries = {
    synara: "dist/index.mjs",
    "synara-restore-migration-backup": "dist/restoreMigrationBackup.mjs",
  };
  if (JSON.stringify(serverPackage.bin ?? {}) !== JSON.stringify(expectedBinaries)) {
    throw new Error(
      "Expected the CLI to expose only the Synara entry point and migration recovery binary.",
    );
  }
  if (SYNARA_PRODUCTION_BUNDLE_ID !== "com.emanueledipietro.synara") {
    throw new Error(`Unexpected production bundle ID: ${SYNARA_PRODUCTION_BUNDLE_ID}.`);
  }
  if (SYNARA_DESKTOP_UPDATE_CHANNEL !== "synara") {
    throw new Error(`Unexpected desktop update channel: ${SYNARA_DESKTOP_UPDATE_CHANNEL}.`);
  }

  const releasePolicy = readReleaseUpdatePolicyConfig(repoRoot);
  const resolvedPolicy = resolveReleaseUpdatePolicy("9.9.9", releasePolicy);
  if (
    resolvedPolicy.lane !== "clean" ||
    !resolvedPolicy.makeLatest ||
    resolvedPolicy.mirrorToStableChannel
  ) {
    throw new Error("Expected stable clean Synara releases to publish on GitHub Latest.");
  }
}

function verifyReleaseWorkflowSafety(): void {
  const workflow = readFileSync(resolve(repoRoot, ".github/workflows/release.yml"), "utf8");
  const rootPackage = JSON.parse(readFileSync(resolve(repoRoot, "package.json"), "utf8")) as {
    scripts?: Record<string, string>;
  };
  if (
    rootPackage.scripts?.["stage6:github:readiness"] !==
    "node scripts/check-stage6-github-release-readiness.ts"
  ) {
    throw new Error("Expected a stable Stage 6 GitHub release-readiness command.");
  }
  if (
    rootPackage.scripts?.["stage6:branch-protection"] !==
    "node scripts/apply-stage6-source-branch-protection.ts"
  ) {
    throw new Error("Expected a stable Stage 6 source-branch protection command.");
  }
  if (
    rootPackage.scripts?.["stage6:environment:baseline"] !==
    "node scripts/apply-stage6-environment-baseline.ts"
  ) {
    throw new Error("Expected a stable Stage 6 GitHub Environment baseline command.");
  }
  const githubReadiness = readFileSync(
    resolve(repoRoot, "scripts/lib/stage6-github-release-readiness.ts"),
    "utf8",
  );
  assertContains(
    githubReadiness,
    '["secret", "list", "-R", input.repository, "--json", "name"]',
    "Expected GitHub readiness to request repository secret names only.",
  );
  assertContains(
    githubReadiness,
    "encodeURIComponent(input.sourceBranch)",
    "Expected GitHub readiness to URL-encode the protected source branch.",
  );
  if (/"secret",\s*"get"/.test(githubReadiness)) {
    throw new Error("GitHub readiness must never request secret values.");
  }
  if ((githubReadiness.match(/"variable",\s*"get"/g) ?? []).length !== 1) {
    throw new Error("GitHub readiness may read only the bounded static release variable value.");
  }
  assertContains(
    githubReadiness,
    'SYNARA_FINALIZE_RELEASE: "1"',
    "Expected GitHub readiness to require the enabled finalization value.",
  );
  assertContains(
    githubReadiness,
    "invalidRepositoryVariableNames",
    "Expected GitHub readiness to report invalid static variable names without exposing values.",
  );
  assertContains(
    githubReadiness,
    "disallowedGaRepositoryVariableNames",
    "Expected GitHub readiness to reject configured unsigned-Windows GA exceptions.",
  );
  const branchProtection = readFileSync(
    resolve(repoRoot, "scripts/lib/stage6-source-branch-protection.ts"),
    "utf8",
  );
  assertContains(
    branchProtection,
    "input.confirmationSha256 !== planSha256",
    "Expected branch-protection writes to require the exact reviewed plan digest.",
  );
  assertContains(
    branchProtection,
    "encodeURIComponent(canonicalPlan.branch)",
    "Expected branch-protection application to URL-encode the exact source branch.",
  );
  assertContains(
    branchProtection,
    "repositoryAdminAuthorityReadBeforeWrite: true",
    "Expected branch-protection application to verify administrator authority before writing.",
  );
  assertContains(
    branchProtection,
    "branchHeadStableAfterWrite: true",
    "Expected branch-protection application to bind and recheck the exact branch HEAD.",
  );
  const environmentBaseline = readFileSync(
    resolve(repoRoot, "scripts/lib/stage6-environment-baseline.ts"),
    "utf8",
  );
  assertContains(
    environmentBaseline,
    "input.confirmationSha256 !== planSha256",
    "Expected Environment baseline writes to require the exact reviewed plan digest.",
  );
  assertContains(
    environmentBaseline,
    "verifyStage6SourceBranchProtectionResponse",
    "Expected Environment baseline writes to require protected source evidence.",
  );
  assertContains(
    environmentBaseline,
    "administratorBypassWriteAvailableInGitHubApi: false",
    "Expected Environment baseline receipts to preserve GitHub's manual administrator-bypass boundary.",
  );
  assertContains(
    environmentBaseline,
    "contains a wait timer or different reviewers that this plan would remove",
    "Expected Environment baseline writes to preserve stronger or different existing controls.",
  );
  const actionUses = [...workflow.matchAll(/^\s+uses:\s+([^#\s]+)(?:\s+#.*)?$/gm)].map(
    (match) => match[1]!,
  );
  const mutableAction = actionUses.find(
    (action) => !action.startsWith("./") && !/@[0-9a-f]{40}$/.test(action),
  );
  if (mutableAction) {
    throw new Error(
      `Release workflow action must be pinned to a full commit SHA: ${mutableAction}.`,
    );
  }
  assertContains(
    workflow,
    "permissions:\n  contents: read\n\nconcurrency:",
    "Expected the release workflow's default token to be read-only.",
  );
  if ((workflow.match(/contents: write/g) ?? []).length !== 1) {
    throw new Error("Only the GitHub Release publication job may receive contents: write.");
  }
  const checkoutCount = actionUses.filter((action) =>
    action.startsWith("actions/checkout@"),
  ).length;
  const readOnlyCheckoutCount = (workflow.match(/persist-credentials: false/g) ?? []).length;
  const appPushCheckoutCount = (workflow.match(/persist-credentials: true/g) ?? []).length;
  if (
    checkoutCount < 2 ||
    readOnlyCheckoutCount !== checkoutCount - 1 ||
    appPushCheckoutCount !== 1
  ) {
    throw new Error(
      "Every non-push checkout must discard credentials; only the explicit release-app version bump may persist them.",
    );
  }
  assertContains(
    workflow,
    "publish_release:\n        description:",
    "Expected a manual publication opt-in input.",
  );
  assertContains(
    workflow,
    "enterprise_ga_candidate:\n        description:",
    "Expected an explicit exact-artifact Enterprise GA mode.",
  );
  assertContains(
    workflow,
    "default: false\n        type: boolean",
    "Expected manual release runs to default to build-only mode.",
  );
  assertContains(
    workflow,
    "publish_release: ${{ steps.release_mode.outputs.publish_release }}",
    "Expected preflight to expose the resolved publication mode.",
  );
  assertContains(
    workflow,
    "name: Verify release-only workflow gates\n        run: bun run release:smoke",
    "Expected the publication workflow to execute its release-only safety gate before building artifacts.",
  );
  assertContains(
    workflow,
    "needs.preflight.outputs.enterprise_ga_candidate != 'true' || needs.stage6_enterprise_approval.result == 'success'",
    "Expected Enterprise GA publication to require the protected approval job.",
  );
  assertContains(
    workflow,
    "needs.build.result == 'success' && needs.finalize_release_assets.result == 'success' && needs.preflight.outputs.publish_release == 'true'",
    "Expected every publication path to retain the explicit publish opt-in plus successful native and finalized-asset boundaries.",
  );
  assertContains(
    workflow,
    '"$ENTERPRISE_GA_CANDIDATE" == "true" && "$PUBLISH_RELEASE" != "true"',
    "Expected Enterprise GA mode to fail closed unless the same run is reserved for publication.",
  );
  assertContains(
    workflow,
    "stage6_enterprise_approval:\n    name: Approve exact Stage 6 Enterprise GA candidate",
    "Expected the exact candidate to pause at a dedicated protected gate.",
  );
  assertContains(
    workflow,
    "name: stage6-enterprise-ga",
    "Expected Enterprise GA approval to use the protected release environment.",
  );
  assertContains(
    workflow,
    '"repos/$GITHUB_REPOSITORY/environments/stage6-enterprise-ga"',
    "Expected the protected gate to download current GitHub environment protection evidence.",
  );
  assertContains(
    workflow,
    '"repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/approvals"',
    "Expected the protected gate to download the actual workflow-run deployment review history.",
  );
  assertContains(
    workflow,
    '"repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"',
    "Expected the protected gate to verify the exact active workflow run and source commit.",
  );
  assertContains(
    workflow,
    '"repos/$GITHUB_REPOSITORY/branches/$source_branch_encoded/protection"',
    "Expected the protected gate to verify the exact run source branch protection.",
  );
  assertContains(
    workflow,
    '--workflow-run-response "$workflow_run_response"',
    "Expected approval creation to consume same-run GitHub evidence.",
  );
  assertContains(
    workflow,
    '--branch-protection-response "$branch_protection_response"',
    "Expected approval creation to consume source-branch protection evidence.",
  );
  assertContains(
    workflow,
    'expected_review_comment="SYNARA_STAGE6_APPROVE $EXPECTED_CANDIDATE_ID $EXPECTED_BUILD_RUN_ID"',
    "Expected the human review comment to bind the exact candidate and fresh workflow run.",
  );
  assertContains(
    workflow,
    '--run-attempt "$RUN_ATTEMPT"',
    "Expected approval verification to reject rerun review-history reuse.",
  );
  assertContains(
    workflow,
    "SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_SHA256",
    "Expected the protected environment to bind the candidate-bundle receipt digest.",
  );
  assertContains(
    workflow,
    "secrets.SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64",
    "Expected protected approval to receive the actual candidate-bundle receipt bytes.",
  );
  assertContains(
    workflow,
    "SYNARA_STAGE6_CANDIDATE_BUILD_RUN_ID",
    "Expected approval to bind the native artifacts to the same workflow run.",
  );
  assertContains(
    workflow,
    "SYNARA_STAGE6_DESKTOP_ARTIFACT_SET_SHA256",
    "Expected protected approval to import the exact Desktop artifact-set digest from candidate evidence.",
  );
  assertContains(
    workflow,
    "name: Download exact candidate Desktop artifacts",
    "Expected protected approval to inspect the artifacts produced by its own workflow run.",
  );
  assertContains(
    workflow,
    "name: Reverify protected Stage 6 Desktop artifact bytes",
    "Expected publication to reverify the approved binary set immediately before feed preparation.",
  );
  assertContains(
    workflow,
    "node scripts/verify-stage6-desktop-artifact-set.ts",
    "Expected the protected workflow to use the bounded Desktop artifact-set verifier.",
  );
  assertContains(
    workflow,
    "node scripts/verify-stage6-candidate-release-binding.ts",
    "Expected protected publication to parse and bind the actual candidate receipt.",
  );
  assertContains(
    workflow,
    "finalize_release_assets:\n    name: Finalize immutable release asset set",
    "Expected updater metadata to be finalized into an immutable artifact before protected approval.",
  );
  assertContains(
    workflow,
    "node scripts/write-release-final-asset-set.ts",
    "Expected the finalizer to bind every public release byte after updater metadata generation.",
  );
  assertContains(
    workflow,
    "node scripts/verify-release-final-asset-set.ts",
    "Expected approval and publication to verify the finalized public asset set.",
  );
  assertContains(
    workflow,
    "--candidate-bundle-receipt stage6-approval/stage6-candidate-bundle-receipt.json",
    "Expected approval creation to consume the verified receipt file in the same process.",
  );
  assertContains(
    workflow,
    '--desktop-artifact-set-sha256 "$DESKTOP_ARTIFACT_SET_SHA256"',
    "Expected the immutable approval record to retain the verified Desktop artifact-set digest.",
  );
  assertContains(
    workflow,
    '--expected-final-asset-set-sha256 "$EXPECTED_FINAL_ASSET_SET_SHA256"',
    "Expected protected approval to bind the exact finalized public asset-set digest.",
  );
  assertContains(
    workflow,
    "--raw-desktop-assets-dir stage6-candidate-artifacts",
    "Expected approval creation to cross-bind raw Desktop bytes to the finalized public assets in one process.",
  );
  assertContains(
    workflow,
    "node scripts/verify-stage6-release-cross-binding.ts",
    "Expected publication to reverify the approved raw-to-final byte relationship.",
  );
  const finalizerStart = workflow.indexOf("finalize_release_assets:");
  const updaterManifestMerge = workflow.indexOf(
    "name: Merge macOS updater manifests before final binding",
  );
  const updaterManifestMergeCommand = workflow.indexOf(
    "node scripts/merge-mac-update-manifests.ts",
  );
  const updaterFeedPreparation = workflow.indexOf(
    "name: Prepare updater feed metadata before final binding",
  );
  const updaterFeedCommand = workflow.indexOf("node scripts/prepare-release-update-feed.ts");
  const finalAssetSetWrite = workflow.indexOf("node scripts/write-release-final-asset-set.ts");
  const finalizedArtifactUpload = workflow.indexOf(
    "name: Upload immutable finalized release assets",
  );
  const approvalArtifactVerification = workflow.indexOf(
    "name: Download exact candidate Desktop artifacts",
  );
  const approvalFinalAssetVerification = workflow.indexOf(
    "node scripts/verify-release-final-asset-set.ts",
    finalizedArtifactUpload + 1,
  );
  const approvalReceiptVerification = workflow.indexOf(
    "node scripts/verify-stage6-candidate-release-binding.ts",
  );
  const approvalRecordWrite = workflow.indexOf("node scripts/write-stage6-release-approval.ts");
  const approvalHistoryFetch = workflow.indexOf(
    '"repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/approvals"',
  );
  const publicationArtifactVerification = workflow.indexOf(
    "name: Reverify protected Stage 6 Desktop artifact bytes",
  );
  const publicationReceiptVerification = workflow.indexOf(
    "node scripts/verify-stage6-candidate-release-binding.ts",
    approvalReceiptVerification + 1,
  );
  const publicationFinalAssetVerification = workflow.indexOf(
    "name: Reverify immutable final release bytes",
  );
  const publicationCrossBindingVerification = workflow.indexOf(
    "node scripts/verify-stage6-release-cross-binding.ts",
  );
  const protectedPublication = workflow.indexOf(
    "name: Publish protected Stage 6 Enterprise GA release",
  );
  if (
    finalizerStart < 0 ||
    updaterManifestMerge <= finalizerStart ||
    updaterManifestMergeCommand <= updaterManifestMerge ||
    updaterFeedPreparation <= updaterManifestMerge ||
    updaterFeedCommand <= updaterFeedPreparation ||
    finalAssetSetWrite <= updaterFeedCommand ||
    finalizedArtifactUpload <= finalAssetSetWrite ||
    approvalArtifactVerification < 0 ||
    approvalArtifactVerification <= finalizedArtifactUpload ||
    approvalReceiptVerification <= approvalArtifactVerification ||
    approvalFinalAssetVerification <= approvalReceiptVerification ||
    approvalRecordWrite <= approvalArtifactVerification ||
    approvalRecordWrite <= approvalFinalAssetVerification ||
    approvalRecordWrite <= approvalReceiptVerification ||
    approvalHistoryFetch <= approvalFinalAssetVerification ||
    approvalRecordWrite <= approvalHistoryFetch ||
    publicationArtifactVerification <= approvalRecordWrite ||
    publicationReceiptVerification <= publicationArtifactVerification ||
    publicationFinalAssetVerification <= approvalRecordWrite ||
    publicationCrossBindingVerification <= publicationFinalAssetVerification ||
    protectedPublication <= publicationCrossBindingVerification ||
    protectedPublication <= publicationFinalAssetVerification ||
    workflow.lastIndexOf("node scripts/merge-mac-update-manifests.ts") !==
      updaterManifestMergeCommand ||
    workflow.lastIndexOf("node scripts/prepare-release-update-feed.ts") !== updaterFeedCommand
  ) {
    throw new Error(
      "Expected updater bytes to finalize once, then the receipt, raw Desktop set and final public set to be verified before authorization and publication.",
    );
  }
  assertContains(
    workflow,
    "node scripts/write-stage6-release-approval.ts",
    "Expected publication to retain a bounded protected-gate record.",
  );
  assertContains(
    workflow,
    "stage6-enterprise-ga-approval.json.sha256",
    "Expected the published release to retain the protected-gate record digest.",
  );
  assertContains(
    workflow,
    "name: Publish protected Stage 6 Enterprise GA release",
    "Expected the protected approval record and candidate payloads to share one release publication step.",
  );
  assertNotContains(
    workflow,
    'gh release upload "$RELEASE_TAG" \\\n            release-assets/stage6-enterprise-ga-approval.json',
    "The protected approval record must not be appended only after the candidate release is public.",
  );
  assertContains(
    workflow,
    "Protected Enterprise GA tag $RELEASE_TAG appeared while evidence review was pending; refusing publication.",
    "Expected the planned tag reservation to be rechecked immediately before publication.",
  );
  assertContains(
    workflow,
    "needs.preflight.outputs.enterprise_ga_candidate != 'true' && vars.SYNARA_PUBLISH_CLI == '1'",
    "Expected protected Enterprise GA candidates to exclude the separately published CLI package.",
  );
  assertNotContains(
    workflow,
    "needs.stage6_enterprise_approval.result == 'success') && vars.SYNARA_PUBLISH_CLI == '1'",
    "Enterprise GA approval must not authorize an npm package outside the candidate evidence bundle.",
  );
  assertContains(
    workflow,
    "needs.preflight.outputs.publish_release == 'true' && vars.SYNARA_FINALIZE_RELEASE == '1'",
    "Expected release finalization to require explicit publication mode.",
  );
  assertContains(
    workflow,
    "SYNARA_PUBLISH_RELEASE: ${{ needs.preflight.outputs.publish_release }}",
    "Expected artifact signing admission to know whether artifacts will be published.",
  );
  assertContains(
    workflow,
    "SYNARA_ENTERPRISE_GA_CANDIDATE: ${{ needs.preflight.outputs.enterprise_ga_candidate }}",
    "Expected signing admission to distinguish protected Enterprise GA artifacts.",
  );
  assertContains(
    workflow,
    "Publishing macOS artifacts requires every signing and notarization secret.",
    "Expected macOS publication to fail closed when signing is unavailable.",
  );
  assertContains(
    workflow,
    'install -m 600 /dev/null "$key_path"',
    "Expected the temporary Apple notary key to be private from creation.",
  );
  assertContains(
    workflow,
    "bun run stage6:desktop:mac:preflight",
    "Expected signed macOS lanes to validate distribution inputs before packaging.",
  );
  assertContains(
    workflow,
    "Publishing Windows artifacts requires every Azure Trusted Signing secret.",
    "Expected Windows publication to fail closed when signing is unavailable.",
  );
  assertNotContains(
    workflow,
    "Windows signing is optional",
    "Windows publication must not retain the unsigned-installer fallback.",
  );
  assertContains(
    workflow,
    "node scripts/verify-release-source-provenance.ts",
    "Expected preflight to bind release source provenance before artifact jobs.",
  );
  const sourceProvenance = readFileSync(
    resolve(repoRoot, "scripts/verify-release-source-provenance.ts"),
    "utf8",
  );
  assertContains(
    sourceProvenance,
    '["ls-remote", "--exit-code", "--tags", "origin", `refs/tags/${tag}`]',
    "Expected protected branch publication to reserve the planned tag against origin.",
  );
  assertNotContains(
    sourceProvenance,
    '["show-ref", "--verify", "--quiet", `refs/tags/${tag}`]',
    "A shallow checkout is not authoritative for planned remote tag absence.",
  );
  assertContains(
    workflow,
    '"$ENTERPRISE_GA_CANDIDATE"',
    "Expected source provenance to bind planned-tag Enterprise GA mode.",
  );
  assertContains(
    workflow,
    "source_commit: ${{ steps.source_provenance.outputs.source_commit }}",
    "Expected the verified source commit to be a preflight output.",
  );
  assertContains(
    workflow,
    "lockfile_sha256: ${{ steps.source_provenance.outputs.lockfile_sha256 }}",
    "Expected the verified lockfile digest to be a preflight output.",
  );
  assertContains(
    workflow,
    '--source-commit "$SOURCE_COMMIT"',
    "Expected desktop packaging to revalidate the verified source commit.",
  );
  assertContains(
    workflow,
    '--lockfile-sha256 "$LOCKFILE_SHA256"',
    "Expected desktop packaging to revalidate the verified lockfile digest.",
  );
  assertNotContains(
    workflow,
    "Align package versions to release version",
    "Release jobs must not mutate package versions after source provenance is established.",
  );
  assertContains(
    workflow,
    "node scripts/write-release-artifact-provenance.ts",
    "Expected every platform lane to prove collected artifacts before upload.",
  );
  assertContains(
    workflow,
    "attestations: write",
    "Expected the release workflow to persist signed artifact attestations.",
  );
  assertContains(
    workflow,
    "artifact-metadata: write",
    "Expected the release workflow to authorize artifact metadata records.",
  );
  assertContains(
    workflow,
    "uses: actions/attest@508db95dd578ae2727ebd6217d5ba78e4fbda05d # v4",
    "Expected every native lane to generate current GitHub artifact provenance.",
  );
  assertContains(
    workflow,
    "subject-path: release-publish/*",
    "Expected artifact attestations to cover the collected release payloads and provenance.",
  );
  assertContains(
    workflow,
    "gh attestation verify",
    "Expected every native lane to verify its primary installer against the repository identity.",
  );
  assertContains(
    workflow,
    '--signer-workflow "$GITHUB_REPOSITORY/.github/workflows/release.yml"',
    "Expected artifact verification to pin the release workflow signer identity.",
  );
  assertContains(
    workflow,
    '--source-digest "$SOURCE_COMMIT"',
    "Expected artifact verification to pin the candidate source commit.",
  );
  assertContains(
    workflow,
    "--deny-self-hosted-runners",
    "Expected native release attestations to require the declared GitHub-hosted builders.",
  );
  assertContains(
    workflow,
    "release-assets/*.attestation*.json",
    "Expected published releases to retain the signed attestation bundles.",
  );
  assertContains(
    workflow,
    'mv release-publish/latest-mac.yml "release-publish/latest-mac-${{ matrix.arch }}.yml"',
    "Expected the x64 macOS matrix lane to preserve a distinct updater manifest for merging.",
  );
  assertContains(
    workflow,
    "APPLE_TEAM_ID: ${{ secrets.APPLE_TEAM_ID }}",
    "Expected macOS signing admission to pin the post-build Team ID.",
  );
  assertContains(
    workflow,
    "AZURE_TRUSTED_SIGNING_SUBJECT_DN: ${{ secrets.AZURE_TRUSTED_SIGNING_SUBJECT_DN }}",
    "Expected Windows signing admission to require the exact certificate subject DN.",
  );
  assertContains(
    workflow,
    '--expected-windows-subject-dn "$EXPECTED_WINDOWS_SUBJECT_DN"',
    "Expected Windows artifact provenance to verify the exact certificate subject DN.",
  );
  assertContains(
    workflow,
    "AZURE_TRUSTED_SIGNING_PUBLISHER_NAME: ${{ secrets.AZURE_TRUSTED_SIGNING_PUBLISHER_NAME }}",
    "Expected the Windows build to receive the publisher identity that is pinned in the bundle.",
  );
  assertContains(
    workflow,
    "node scripts/verify-packaged-desktop-startup.ts",
    "Expected every native payload to pass isolated packaged startup before upload.",
  );

  const cliScript = readFileSync(resolve(repoRoot, "apps/server/scripts/cli.ts"), "utf8");
  assertContains(
    cliScript,
    "makeTempDirectoryScoped",
    "Expected CLI publication to build an exclusively owned temporary package tree.",
  );
  assertContains(
    cliScript,
    "cwd: stagedPackageDir",
    "Expected npm publication to run only from the isolated CLI stage.",
  );
  assertContains(
    cliScript,
    "Staged CLI bin target is missing its Node shebang",
    "Expected staged CLI commands to remain executable npm bin entries.",
  );
  assertNotContains(
    cliScript,
    ".publish-bak",
    "CLI publication must not mutate and restore source-tree assets.",
  );

  const desktopBuildConfig = readFileSync(
    resolve(repoRoot, "apps/desktop/tsdown.config.mts"),
    "utf8",
  );
  assertContains(
    desktopBuildConfig,
    "__SYNARA_WINDOWS_UPDATER_PUBLISHER__",
    "Expected the Windows updater publisher identity to be compiled into the main bundle.",
  );

  const updaterSecurity = readFileSync(
    resolve(repoRoot, "apps/desktop/src/electronUpdaterSecurity.ts"),
    "utf8",
  );
  assertNotContains(
    updaterSecurity,
    "return feedPublisherNames",
    "Runtime signature verification must not trust publisher names from mutable updater config.",
  );
}

function verifyDesktopStageLockAuthority(): void {
  const buildScript = readFileSync(resolve(repoRoot, "scripts/build-desktop-artifact.ts"), "utf8");
  const gitAttributes = readFileSync(resolve(repoRoot, ".gitattributes"), "utf8");
  assertContains(
    gitAttributes,
    "bun.lock text eol=lf",
    "Expected bun.lock to retain byte-identical LF endings on every release runner.",
  );
  assertContains(
    buildScript,
    "bun install --frozen-lockfile --ignore-scripts --linker hoisted",
    "Expected macOS and Linux desktop staging to install from the repository's frozen workspace lockfile.",
  );
  assertContains(
    buildScript,
    'if (platform === "win")',
    "Expected Windows staging to use its explicit Bun lockfile-workaround path.",
  );
  assertContains(
    buildScript,
    "bun install --omit=dev --ignore-scripts --linker hoisted",
    "Expected Windows staging to omit dev dependencies without Bun's implicitly frozen production mode.",
  );
  assertNotContains(
    buildScript,
    "--production --frozen-lockfile",
    "Desktop staging must avoid Bun's divergent frozen production-workspace lockfile resolution.",
  );
  assertNotContains(
    buildScript,
    "bun install --production",
    "Windows staging must not use Bun's production flag because it implicitly forces frozen mode.",
  );
  assertNotContains(
    buildScript,
    "--filter @synara/",
    "Desktop staging must not use Bun workspace filters because filtered hoisted installs can diverge from bun.lock.",
  );
  assertContains(
    buildScript,
    ")`npm rebuild node-pty --foreground-scripts`,",
    "Expected Linux desktop staging to build only node-pty after the script-free frozen install.",
  );
  assertNotContains(
    buildScript,
    "npm rebuild --foreground-scripts",
    "Desktop staging must never enable every dependency lifecycle script.",
  );
  assertNotContains(
    buildScript,
    "bun pm trust --all",
    "Desktop staging must never trust every dependency lifecycle script.",
  );
  assertContains(
    buildScript,
    "import('@effect/platform-node/NodeRedis')",
    "Expected the frozen Desktop stage to import the Redis peer-dependency path before packaging.",
  );
  assertContains(
    buildScript,
    "validateMacDistributionEnvironment({ environment: process.env })",
    "Expected direct signed macOS builds to fail before staging when distribution inputs are unsafe.",
  );
  assertContains(
    buildScript,
    'createRequire(new URL("./package.json", import.meta.url))',
    "Expected desktop packaging to resolve dependencies from the owning scripts workspace.",
  );
  assertContains(
    buildScript,
    'requireFromScriptsWorkspace.resolve("electron-builder/cli.js")',
    "Expected desktop packaging to resolve electron-builder across Bun hoisting layouts.",
  );
  assertContains(
    buildScript,
    "`${process.execPath} ${electronBuilderCliPath}",
    "Expected desktop packaging to invoke electron-builder through Node without platform-specific bin shims.",
  );
  assertNotContains(
    buildScript,
    "electron-builder.cmd",
    "Desktop packaging must not depend on a Windows bin shim that Bun may hoist elsewhere.",
  );
  assertContains(
    buildScript,
    "synaraCommitHash: commitHash",
    "Expected the staged package to carry its exact source commit.",
  );
  assertContains(
    buildScript,
    "synaraLockfileSha256: resolvedLockfileSha256",
    "Expected the staged package to carry its repository lockfile digest.",
  );
  assertContains(
    buildScript,
    "synaraWindowsPublisherSubject: resolvedBuildConfig.windowsPublisherSubject",
    "Expected signed Windows packages to carry the independently configured certificate subject DN.",
  );

  const lockfile = readFileSync(resolve(repoRoot, RELEASE_LOCKFILE_PATH), "utf8");
  const packagesSectionOffset = lockfile.indexOf('\n  "packages": {');
  if (packagesSectionOffset < 0) {
    throw new Error("Expected bun.lock to contain a packages section.");
  }
  const workspaceImporters = lockfile.slice(0, packagesSectionOffset);
  for (const manifestPath of RELEASE_WORKSPACE_MANIFEST_PATHS) {
    const workspacePath = manifestPath === "package.json" ? "" : dirname(manifestPath);
    if (!workspaceImporters.includes(`${JSON.stringify(workspacePath)}: {`)) {
      throw new Error(`Expected ${manifestPath} to have a matching importer in bun.lock.`);
    }
  }
}

const tempRoot = mkdtempSync(join(tmpdir(), "synara-release-smoke-"));

try {
  verifyCanonicalIdentity();
  verifyReleaseWorkflowSafety();
  verifyDesktopStageLockAuthority();
  copyWorkspaceManifestFixture(tempRoot);

  execFileSync(
    process.execPath,
    [
      resolve(repoRoot, "scripts/update-release-package-versions.ts"),
      "9.9.9-smoke.0",
      "--root",
      tempRoot,
    ],
    {
      cwd: repoRoot,
      stdio: "inherit",
    },
  );

  execFileSync("bun", ["install", "--lockfile-only", "--ignore-scripts"], {
    cwd: tempRoot,
    stdio: "inherit",
  });

  const lockfile = readFileSync(resolve(tempRoot, "bun.lock"), "utf8");
  assertContains(
    lockfile,
    `"version": "9.9.9-smoke.0"`,
    "Expected bun.lock to contain the smoke version.",
  );

  const { arm64Path, x64Path } = writeMacManifestFixtures(tempRoot);
  execFileSync(
    process.execPath,
    [resolve(repoRoot, "scripts/merge-mac-update-manifests.ts"), arm64Path, x64Path],
    {
      cwd: repoRoot,
      stdio: "inherit",
    },
  );

  const mergedManifest = readFileSync(arm64Path, "utf8");
  assertContains(
    mergedManifest,
    "Synara-9.9.9-smoke.0-arm64.zip",
    "Merged manifest is missing the arm64 asset.",
  );
  assertContains(
    mergedManifest,
    "Synara-9.9.9-smoke.0-x64.zip",
    "Merged manifest is missing the x64 asset.",
  );
  assertNotContains(
    mergedManifest,
    ".dmg",
    "macOS updater manifests must describe only the finalized ZIP artifacts.",
  );

  console.log("Release smoke checks passed.");
} finally {
  rmSync(tempRoot, { recursive: true, force: true });
}
