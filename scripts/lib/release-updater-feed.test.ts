import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";

import { verifyReleaseUpdaterFeed } from "./release-updater-feed.ts";

const roots: string[] = [];
const version = "1.2.3";
const updateChannel = "synara";

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function metadata(bytes: string): { readonly size: number; readonly sha512: string } {
  return {
    size: Buffer.byteLength(bytes),
    sha512: createHash("sha512").update(bytes).digest("base64"),
  };
}

function manifest(
  manifestVersion: string,
  files: ReadonlyArray<{
    readonly url: string;
    readonly bytes: string;
    readonly blockMapSize?: number;
    readonly sizeOverride?: number;
    readonly sha512Override?: string;
  }>,
  topLevel = true,
): string {
  const lines = [`version: ${manifestVersion}`, "files:"];
  for (const file of files) {
    const digest = metadata(file.bytes);
    lines.push(`  - url: '${file.url.replaceAll("'", "''")}'`);
    lines.push(`    sha512: ${file.sha512Override ?? digest.sha512}`);
    lines.push(`    size: ${file.sizeOverride ?? digest.size}`);
    if (file.blockMapSize !== undefined) lines.push(`    blockMapSize: ${file.blockMapSize}`);
  }
  if (topLevel) {
    const first = files[0]!;
    const digest = metadata(first.bytes);
    lines.push(`path: '${first.url.replaceAll("'", "''")}'`);
    lines.push(`sha512: ${first.sha512Override ?? digest.sha512}`);
  }
  lines.push("releaseDate: '2026-07-31T00:00:00.000Z'", "");
  return lines.join("\n");
}

interface Fixture {
  readonly root: string;
  readonly payloads: {
    readonly macArm64: { readonly name: string; readonly bytes: string };
    readonly macX64: { readonly name: string; readonly bytes: string };
    readonly windows: { readonly name: string; readonly bytes: string };
    readonly linux: { readonly name: string; readonly bytes: string };
    readonly windowsBlockmap: { readonly name: string; readonly bytes: string };
  };
}

function writePair(root: string, defaultName: string, channelName: string, bytes: string): void {
  writeFileSync(join(root, defaultName), bytes);
  writeFileSync(join(root, channelName), bytes);
}

function createFixture(): Fixture {
  const root = mkdtempSync(join(tmpdir(), "synara-updater-feed-"));
  roots.push(root);
  const payloads = {
    macArm64: { name: "Synara-1.2.3-arm64-mac.zip", bytes: "mac-arm64-zip" },
    macX64: { name: "Synara-1.2.3-x64-mac.zip", bytes: "mac-x64-zip" },
    windows: { name: "Synara-Setup-1.2.3.exe", bytes: "windows-exe" },
    linux: { name: "Synara-1.2.3-x64.AppImage", bytes: "linux-appimage" },
    windowsBlockmap: {
      name: "Synara-Setup-1.2.3.exe.blockmap",
      bytes: "windows-blockmap",
    },
  } as const;
  for (const payload of Object.values(payloads)) {
    writeFileSync(join(root, payload.name), payload.bytes);
  }
  writePair(
    root,
    "latest-mac.yml",
    "synara-mac.yml",
    manifest(
      version,
      [
        { url: payloads.macArm64.name, bytes: payloads.macArm64.bytes },
        { url: payloads.macX64.name, bytes: payloads.macX64.bytes },
      ],
      false,
    ),
  );
  writePair(
    root,
    "latest.yml",
    "synara.yml",
    manifest(version, [
      {
        url: payloads.windows.name,
        bytes: payloads.windows.bytes,
        blockMapSize: Buffer.byteLength(payloads.windowsBlockmap.bytes),
      },
    ]),
  );
  writePair(
    root,
    "latest-linux.yml",
    "synara-linux.yml",
    manifest(version, [{ url: payloads.linux.name, bytes: payloads.linux.bytes }]),
  );
  return { root, payloads };
}

describe("final release updater feed", () => {
  it("binds aliases and every updater payload to streaming size and SHA-512", () => {
    const fixture = createFixture();
    const result = verifyReleaseUpdaterFeed({
      assetsDirectory: fixture.root,
      version,
      updateChannel,
    });
    expect(result.manifests).toHaveLength(3);
    expect(result.payloads.map((payload) => `${payload.platform}/${payload.arch}`)).toEqual([
      "linux/x64",
      "mac/arm64",
      "mac/x64",
      "windows/x64",
    ]);
    expect(result.updaterFeedSha256).toMatch(/^sha256:[0-9a-f]{64}$/);
  });

  it("rejects a stale release version", () => {
    const fixture = createFixture();
    const stale = readFileSync(join(fixture.root, "latest-mac.yml"), "utf8").replace(
      "version: 1.2.3",
      "version: 1.2.2",
    );
    writePair(fixture.root, "latest-mac.yml", "synara-mac.yml", stale);
    expect(() =>
      verifyReleaseUpdaterFeed({ assetsDirectory: fixture.root, version, updateChannel }),
    ).toThrow("does not match release");
  });

  it("rejects a payload SHA-512 mismatch", () => {
    const fixture = createFixture();
    const wrongSha512 = Buffer.alloc(64, 7).toString("base64");
    const windows = manifest(version, [
      {
        url: fixture.payloads.windows.name,
        bytes: fixture.payloads.windows.bytes,
        blockMapSize: Buffer.byteLength(fixture.payloads.windowsBlockmap.bytes),
        sha512Override: wrongSha512,
      },
    ]);
    writePair(fixture.root, "latest.yml", "synara.yml", windows);
    expect(() =>
      verifyReleaseUpdaterFeed({ assetsDirectory: fixture.root, version, updateChannel }),
    ).toThrow("SHA-512 does not match");
  });

  it("rejects a payload size mismatch", () => {
    const fixture = createFixture();
    const linux = manifest(version, [
      {
        url: fixture.payloads.linux.name,
        bytes: fixture.payloads.linux.bytes,
        sizeOverride: 999,
      },
    ]);
    writePair(fixture.root, "latest-linux.yml", "synara-linux.yml", linux);
    expect(() =>
      verifyReleaseUpdaterFeed({ assetsDirectory: fixture.root, version, updateChannel }),
    ).toThrow("payload size does not match");
  });

  it("rejects a missing macOS architecture", () => {
    const fixture = createFixture();
    const arm64Only = manifest(
      version,
      [{ url: fixture.payloads.macArm64.name, bytes: fixture.payloads.macArm64.bytes }],
      false,
    );
    writePair(fixture.root, "latest-mac.yml", "synara-mac.yml", arm64Only);
    expect(() =>
      verifyReleaseUpdaterFeed({ assetsDirectory: fixture.root, version, updateChannel }),
    ).toThrow("exactly the arm64 and x64 ZIP payloads");
  });

  it("rejects latest/channel alias drift", () => {
    const fixture = createFixture();
    writeFileSync(join(fixture.root, "synara-linux.yml"), "different");
    expect(() =>
      verifyReleaseUpdaterFeed({ assetsDirectory: fixture.root, version, updateChannel }),
    ).toThrow("byte-identical aliases");
  });

  it.each(["../Synara-Setup-1.2.3.exe", "https://example.com/Synara-Setup-1.2.3.exe"])(
    "rejects unsafe updater URL %s",
    (url) => {
      const fixture = createFixture();
      const windows = manifest(version, [
        {
          url,
          bytes: fixture.payloads.windows.bytes,
          blockMapSize: Buffer.byteLength(fixture.payloads.windowsBlockmap.bytes),
        },
      ]);
      writePair(fixture.root, "latest.yml", "synara.yml", windows);
      expect(() =>
        verifyReleaseUpdaterFeed({ assetsDirectory: fixture.root, version, updateChannel }),
      ).toThrow("safe public asset basename");
    },
  );

  it("rejects an incorrect blockmap size", () => {
    const fixture = createFixture();
    const windows = manifest(version, [
      {
        url: fixture.payloads.windows.name,
        bytes: fixture.payloads.windows.bytes,
        blockMapSize: Buffer.byteLength(fixture.payloads.windowsBlockmap.bytes) + 1,
      },
    ]);
    writePair(fixture.root, "latest.yml", "synara.yml", windows);
    expect(() =>
      verifyReleaseUpdaterFeed({ assetsDirectory: fixture.root, version, updateChannel }),
    ).toThrow("blockMapSize does not match");
  });
});
