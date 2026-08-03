import { createHash } from "node:crypto";
import { mkdtempSync, renameSync, rmSync, symlinkSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import {
  RELEASE_FINAL_ASSET_SET_MANIFEST,
  RELEASE_FINAL_ASSET_SET_SIDECAR,
  assertVerifiedReleaseFinalAssetSet,
  verifyReleaseFinalAssetSet,
  writeReleaseFinalAssetSet,
} from "./release-final-asset-set.ts";

const roots: string[] = [];
const identity = {
  sourceTag: "v1.2.3",
  sourceCommit: "a".repeat(40),
  lockfileSha256: "b".repeat(64),
  version: "1.2.3",
  updateChannel: "synara",
} as const;

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function updateFileEntry(fileName: string, blockMapSize?: number): string {
  const bytes = `${fileName}-bytes`;
  const sha512 = createHash("sha512").update(bytes).digest("base64");
  return [
    `  - url: ${fileName}`,
    `    sha512: ${sha512}`,
    `    size: ${Buffer.byteLength(bytes)}`,
    ...(blockMapSize === undefined ? [] : [`    blockMapSize: ${blockMapSize}`]),
  ].join("\n");
}

function createAssets(): string {
  const root = mkdtempSync(join(tmpdir(), "synara-final-assets-"));
  roots.push(root);
  const payloads = [
    "Synara-1.2.3-arm64.dmg",
    "Synara-1.2.3-x64.dmg",
    "Synara-1.2.3-arm64-mac.zip",
    "Synara-1.2.3-x64-mac.zip",
    "Synara-1.2.3-x64.AppImage",
    "Synara-Setup-1.2.3.exe",
    "Synara-Setup-1.2.3.exe.blockmap",
  ];
  const targets = ["linux-x64", "mac-arm64", "mac-x64", "win-x64"];
  const trust = targets.flatMap((target) => [
    `artifact-${target}.provenance.json`,
    `artifact-${target}.attestation.json`,
    `artifact-${target}.attestation-verification.json`,
  ]);
  for (const fileName of [...payloads, ...trust]) {
    writeFileSync(join(root, fileName), `${fileName}-bytes`);
  }
  const writePair = (defaultName: string, channelName: string, bytes: string): void => {
    writeFileSync(join(root, defaultName), bytes);
    writeFileSync(join(root, channelName), bytes);
  };
  writePair(
    "latest-mac.yml",
    "synara-mac.yml",
    `version: 1.2.3\nfiles:\n${updateFileEntry("Synara-1.2.3-arm64-mac.zip")}\n${updateFileEntry("Synara-1.2.3-x64-mac.zip")}\nreleaseDate: '2026-07-31T00:00:00.000Z'\n`,
  );
  const windowsSha512 = createHash("sha512")
    .update("Synara-Setup-1.2.3.exe-bytes")
    .digest("base64");
  writePair(
    "latest.yml",
    "synara.yml",
    `version: 1.2.3\nfiles:\n${updateFileEntry("Synara-Setup-1.2.3.exe", Buffer.byteLength("Synara-Setup-1.2.3.exe.blockmap-bytes"))}\npath: Synara-Setup-1.2.3.exe\nsha512: ${windowsSha512}\nreleaseDate: '2026-07-31T00:00:00.000Z'\n`,
  );
  const linuxSha512 = createHash("sha512")
    .update("Synara-1.2.3-x64.AppImage-bytes")
    .digest("base64");
  writePair(
    "latest-linux.yml",
    "synara-linux.yml",
    `version: 1.2.3\nfiles:\n${updateFileEntry("Synara-1.2.3-x64.AppImage")}\npath: Synara-1.2.3-x64.AppImage\nsha512: ${linuxSha512}\nreleaseDate: '2026-07-31T00:00:00.000Z'\n`,
  );
  return root;
}

function writeAndVerify(root: string) {
  const written = writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity });
  return {
    written,
    verified: verifyReleaseFinalAssetSet({
      assetsDirectory: root,
      ...identity,
      expectedFinalAssetSetSha256: written.manifest.finalAssetSetSha256,
    }),
  };
}

describe("release final public asset set", () => {
  it("writes a canonical manifest and verifies every final byte", () => {
    const root = createAssets();
    const { written, verified } = writeAndVerify(root);
    expect(written.manifest.files.map((file) => file.fileName)).toEqual(
      written.manifest.files.map((file) => file.fileName).toSorted(),
    );
    expect(written.manifest.finalAssetSetSha256).toMatch(/^sha256:[0-9a-f]{64}$/);
    expect(written.manifest.updaterFeed.sha256).toMatch(/^sha256:[0-9a-f]{64}$/);
    expect(() => assertVerifiedReleaseFinalAssetSet(verified)).not.toThrow();
    expect(() => assertVerifiedReleaseFinalAssetSet({ ...verified })).toThrow(
      "not produced by the local byte verifier",
    );
  });

  it("refuses to overwrite an existing final manifest", () => {
    const root = createAssets();
    writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity });
    expect(() => writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity })).toThrow(
      "will not be overwritten",
    );
  });

  it("rejects a payload mutation after finalization", () => {
    const root = createAssets();
    writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity });
    writeFileSync(join(root, "Synara-1.2.3-arm64.dmg"), "mutated");
    expect(() => verifyReleaseFinalAssetSet({ assetsDirectory: root, ...identity })).toThrow(
      "do not match the bound manifest",
    );
  });

  it("rejects an asset added after finalization", () => {
    const root = createAssets();
    writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity });
    writeFileSync(join(root, "extra.zip"), "extra");
    expect(() => verifyReleaseFinalAssetSet({ assetsDirectory: root, ...identity })).toThrow(
      "do not exactly match the updater manifests",
    );
  });

  it("rejects a missing asset", () => {
    const root = createAssets();
    writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity });
    unlinkSync(join(root, "Synara-1.2.3-x64.dmg"));
    expect(() => verifyReleaseFinalAssetSet({ assetsDirectory: root, ...identity })).toThrow(
      "minimum cross-platform installer",
    );
  });

  it("rejects a symlink substituted for a public file", () => {
    const root = createAssets();
    writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity });
    unlinkSync(join(root, "Synara-1.2.3-arm64.dmg"));
    symlinkSync("Synara-1.2.3-x64.dmg", join(root, "Synara-1.2.3-arm64.dmg"));
    expect(() => verifyReleaseFinalAssetSet({ assetsDirectory: root, ...identity })).toThrow(
      "regular non-symlink file",
    );
  });

  it("rejects a renamed required trust file", () => {
    const root = createAssets();
    renameSync(
      join(root, "artifact-linux-x64.provenance.json"),
      join(root, "artifact-linux-x64.provenance-renamed.json"),
    );
    expect(() => writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity })).toThrow(
      "exact trust evidence",
    );
  });

  it("rejects the stale x64 mac updater manifest", () => {
    const root = createAssets();
    writeFileSync(join(root, "latest-mac-x64.yml"), "stale");
    expect(() => writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity })).toThrow(
      "must be removed",
    );
  });

  it("rejects source identity drift", () => {
    const root = createAssets();
    writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity });
    expect(() =>
      verifyReleaseFinalAssetSet({
        assetsDirectory: root,
        ...identity,
        sourceCommit: "c".repeat(40),
      }),
    ).toThrow("identity does not match");
  });

  it("rejects manifest and sidecar removal or substitution", () => {
    const root = createAssets();
    writeReleaseFinalAssetSet({ assetsDirectory: root, ...identity });
    unlinkSync(join(root, RELEASE_FINAL_ASSET_SET_SIDECAR));
    expect(() => verifyReleaseFinalAssetSet({ assetsDirectory: root, ...identity })).toThrow();
    writeFileSync(join(root, RELEASE_FINAL_ASSET_SET_SIDECAR), "bad");
    expect(() => verifyReleaseFinalAssetSet({ assetsDirectory: root, ...identity })).toThrow(
      "sidecar does not match",
    );
    expect(RELEASE_FINAL_ASSET_SET_MANIFEST).toBe("release-final-asset-set.json");
  });
});
