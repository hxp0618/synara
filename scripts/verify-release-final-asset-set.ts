#!/usr/bin/env node

import { appendFileSync } from "node:fs";
import { verifyReleaseFinalAssetSet } from "./lib/release-final-asset-set.ts";

function parseArgs(argv: ReadonlyArray<string>): Map<string, string> {
  const values = new Map<string, string>();
  const known = new Set([
    "--assets-dir",
    "--source-tag",
    "--source-commit",
    "--lockfile-sha256",
    "--version",
    "--update-channel",
    "--expected-final-asset-set-sha256",
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
const expectedFinalAssetSetSha256 = values.get("--expected-final-asset-set-sha256");
const verified = verifyReleaseFinalAssetSet({
  assetsDirectory: required("--assets-dir"),
  sourceTag: required("--source-tag"),
  sourceCommit: required("--source-commit"),
  lockfileSha256: required("--lockfile-sha256"),
  version: required("--version"),
  updateChannel: required("--update-channel"),
  ...(expectedFinalAssetSetSha256 ? { expectedFinalAssetSetSha256 } : {}),
});
console.log(JSON.stringify(verified, null, 2));
if (process.env.GITHUB_OUTPUT) {
  appendFileSync(
    process.env.GITHUB_OUTPUT,
    `verified_final_asset_set_sha256=${verified.finalAssetSetSha256}\nverified_updater_feed_sha256=${verified.updaterFeed.sha256}\n`,
  );
}
