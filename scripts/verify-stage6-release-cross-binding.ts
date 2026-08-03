#!/usr/bin/env node

import { verifyReleaseFinalAssetSet } from "./lib/release-final-asset-set.ts";
import { verifyStage6DesktopArtifactSet } from "./lib/stage6-desktop-artifact-set.ts";
import { verifyStage6ReleaseCrossBinding } from "./lib/stage6-release-cross-binding.ts";

function parseArgs(argv: ReadonlyArray<string>): Map<string, string> {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name?.startsWith("--") || value === undefined) {
      throw new Error(`Invalid Stage 6 release cross-binding argument near ${name ?? "<end>"}.`);
    }
    if (values.has(name))
      throw new Error(`Duplicate Stage 6 release cross-binding argument: ${name}.`);
    values.set(name, value);
  }
  const known = new Set([
    "--raw-assets-dir",
    "--final-assets-dir",
    "--candidate-tag",
    "--source-commit",
    "--lockfile-sha256",
    "--desktop-artifact-set-sha256",
    "--final-asset-set-sha256",
    "--release-version",
    "--update-channel",
    "--expected-cross-binding-sha256",
  ]);
  for (const name of values.keys()) {
    if (!known.has(name))
      throw new Error(`Unknown Stage 6 release cross-binding argument: ${name}.`);
  }
  return values;
}

const values = parseArgs(process.argv.slice(2));
const required = (name: string): string => {
  const value = values.get(name);
  if (!value) throw new Error(`Missing Stage 6 release cross-binding argument: ${name}.`);
  return value;
};

const raw = verifyStage6DesktopArtifactSet({
  assetsDirectory: required("--raw-assets-dir"),
  candidateTag: required("--candidate-tag"),
  sourceCommit: required("--source-commit"),
  lockfileSha256: required("--lockfile-sha256"),
  expectedArtifactSetSha256: required("--desktop-artifact-set-sha256"),
});
const finalAssets = verifyReleaseFinalAssetSet({
  assetsDirectory: required("--final-assets-dir"),
  sourceTag: required("--candidate-tag"),
  sourceCommit: required("--source-commit"),
  lockfileSha256: required("--lockfile-sha256"),
  version: required("--release-version"),
  updateChannel: required("--update-channel"),
  expectedFinalAssetSetSha256: required("--final-asset-set-sha256"),
});
const crossBinding = verifyStage6ReleaseCrossBinding(raw, finalAssets);
if (crossBinding.crossBindingSha256 !== required("--expected-cross-binding-sha256")) {
  throw new Error("Stage 6 raw-to-final release cross-binding digest does not match approval.");
}
console.log(JSON.stringify(crossBinding, null, 2));
