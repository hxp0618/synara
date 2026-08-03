#!/usr/bin/env node

import { appendFileSync } from "node:fs";
import { writeReleaseFinalAssetSet } from "./lib/release-final-asset-set.ts";

function parseArgs(argv: ReadonlyArray<string>): Map<string, string> {
  const values = new Map<string, string>();
  const known = new Set([
    "--assets-dir",
    "--source-tag",
    "--source-commit",
    "--lockfile-sha256",
    "--version",
    "--update-channel",
  ]);
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name?.startsWith("--") || value === undefined) {
      throw new Error(`Invalid release final asset-set argument near ${name ?? "<end>"}.`);
    }
    if (!known.has(name)) throw new Error(`Unknown release final asset-set argument: ${name}.`);
    if (values.has(name)) throw new Error(`Duplicate release final asset-set argument: ${name}.`);
    values.set(name, value);
  }
  return values;
}

const values = parseArgs(process.argv.slice(2));
const required = (name: string): string => {
  const value = values.get(name);
  if (!value) throw new Error(`Missing release final asset-set argument: ${name}.`);
  return value;
};
const result = writeReleaseFinalAssetSet({
  assetsDirectory: required("--assets-dir"),
  sourceTag: required("--source-tag"),
  sourceCommit: required("--source-commit"),
  lockfileSha256: required("--lockfile-sha256"),
  version: required("--version"),
  updateChannel: required("--update-channel"),
});
const output = {
  manifestPath: result.manifestPath,
  sidecarPath: result.sidecarPath,
  finalAssetSetSha256: result.manifest.finalAssetSetSha256,
  updaterFeedSha256: result.manifest.updaterFeed.sha256,
};
console.log(JSON.stringify(output, null, 2));
if (process.env.GITHUB_OUTPUT) {
  if (result.manifestPath.includes("\n") || result.manifestPath.includes("\r")) {
    throw new Error("Release final asset-set manifest path cannot contain a line break.");
  }
  appendFileSync(
    process.env.GITHUB_OUTPUT,
    `final_asset_set_sha256=${result.manifest.finalAssetSetSha256}\nupdater_feed_sha256=${result.manifest.updaterFeed.sha256}\nfinal_asset_set_manifest_path=${result.manifestPath}\n`,
  );
}
