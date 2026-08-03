import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, describe, expect, it } from "vitest";

import {
  type CommandResult,
  resolveExtractedSynaraApp,
  resolveMountedSynaraApp,
  verifyMacDiskImagePayload,
  verifyMacZipApp,
  writeReleaseArtifactProvenance,
} from "./release-artifact-provenance.ts";

const temporaryRoots: string[] = [];

afterEach(() => {
  for (const root of temporaryRoots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
});

function createAssets(): string {
  const root = mkdtempSync(join(tmpdir(), "synara-artifact-provenance-test-"));
  temporaryRoots.push(root);
  writeFileSync(join(root, "Synara-1.2.3-x64.AppImage"), "app-image-bytes");
  writeFileSync(join(root, "latest-linux.yml"), "version: 1.2.3\n");
  return root;
}

function createWindowsAssets(): string {
  const root = mkdtempSync(join(tmpdir(), "synara-windows-provenance-test-"));
  temporaryRoots.push(root);
  writeFileSync(join(root, "Synara-1.2.3-x64.exe"), "unsigned-windows-bytes");
  writeFileSync(join(root, "latest.yml"), "version: 1.2.3\n");
  return root;
}

function successfulCommand(): CommandResult {
  return { stdout: "", stderr: "" };
}

describe("release artifact provenance", () => {
  it("hashes the exact collected Linux assets into a deterministic manifest", async () => {
    const assetsDirectory = createAssets();
    const result = await writeReleaseArtifactProvenance({
      assetsDirectory,
      platform: "linux",
      arch: "x64",
      target: "AppImage",
      version: "1.2.3",
      sourceCommit: "a".repeat(40),
      sourceTag: null,
      lockfileSha256: "b".repeat(64),
      publication: false,
      signed: false,
    });

    expect(result.path).toBe(join(assetsDirectory, "artifact-linux-x64.provenance.json"));
    expect(result.manifest.target).toBe("AppImage");
    expect(result.manifest.signing).toEqual({
      status: "not-applicable",
      scheme: "none",
      identity: null,
      checks: ["AppImage payload present"],
    });
    expect(result.manifest.artifacts.map((artifact) => artifact.fileName)).toEqual([
      "Synara-1.2.3-x64.AppImage",
    ]);
    expect(result.manifest.artifacts.some((artifact) => artifact.fileName.endsWith(".yml"))).toBe(
      false,
    );
    expect(
      result.manifest.artifacts.find(
        (artifact) => artifact.fileName === "Synara-1.2.3-x64.AppImage",
      )?.sha256,
    ).toBe(createHash("sha256").update("app-image-bytes").digest("hex"));
    expect(JSON.parse(readFileSync(result.path, "utf8"))).toEqual(result.manifest);
  });

  it("rejects publication without an exact source tag", async () => {
    await expect(
      writeReleaseArtifactProvenance({
        assetsDirectory: createAssets(),
        platform: "linux",
        arch: "x64",
        target: "AppImage",
        version: "1.2.3",
        sourceCommit: "a".repeat(40),
        sourceTag: null,
        lockfileSha256: "b".repeat(64),
        publication: true,
        signed: false,
      }),
    ).rejects.toThrow("requires an exact source tag");
  });

  it("records an explicit version-scoped unsigned Windows publication", async () => {
    const result = await writeReleaseArtifactProvenance({
      assetsDirectory: createWindowsAssets(),
      platform: "win",
      arch: "x64",
      target: "nsis",
      version: "1.2.3",
      sourceCommit: "a".repeat(40),
      sourceTag: "v1.2.3",
      lockfileSha256: "b".repeat(64),
      publication: true,
      signed: false,
      allowUnsignedWindowsPublication: true,
    });

    expect(result.manifest.signing).toEqual({
      status: "unsigned-explicit-release",
      scheme: "none",
      identity: null,
      checks: ["explicit version-scoped Windows release exception"],
    });
  });

  it("still rejects unsigned Windows publication without the explicit exception", async () => {
    await expect(
      writeReleaseArtifactProvenance({
        assetsDirectory: createWindowsAssets(),
        platform: "win",
        arch: "x64",
        target: "nsis",
        version: "1.2.3",
        sourceCommit: "a".repeat(40),
        sourceTag: "v1.2.3",
        lockfileSha256: "b".repeat(64),
        publication: true,
        signed: false,
      }),
    ).rejects.toThrow("requires verified signing");
  });

  it("accepts only a real top-level Synara.app in a mounted disk image", () => {
    const root = mkdtempSync(join(tmpdir(), "synara-mounted-dmg-test-"));
    temporaryRoots.push(root);
    mkdirSync(join(root, "Synara.app"));
    expect(resolveMountedSynaraApp(root)).toBe(join(root, "Synara.app"));

    mkdirSync(join(root, "Unexpected.app"));
    expect(() => resolveMountedSynaraApp(root)).toThrow(
      "Expected exactly one top-level app bundle in mounted DMG, found 2.",
    );
  });

  it("rejects a symlink masquerading as the mounted Synara.app", () => {
    const root = mkdtempSync(join(tmpdir(), "synara-mounted-dmg-symlink-test-"));
    temporaryRoots.push(root);
    const target = join(root, "payload");
    mkdirSync(target);
    symlinkSync(target, join(root, "Synara.app"));
    expect(() => resolveMountedSynaraApp(root)).toThrow("must be a real directory");
  });

  it("requires the extracted ZIP payload to contain one real top-level Synara.app", () => {
    const root = mkdtempSync(join(tmpdir(), "synara-extracted-zip-test-"));
    temporaryRoots.push(root);
    const outside = mkdtempSync(join(tmpdir(), "synara-extracted-zip-outside-test-"));
    temporaryRoots.push(outside);
    symlinkSync(outside, join(root, "Synara.app"));

    expect(() => resolveExtractedSynaraApp(root)).toThrow("must be a real directory");

    rmSync(join(root, "Synara.app"));
    mkdirSync(join(root, "Renamed.app"));
    expect(() => resolveExtractedSynaraApp(root)).toThrow(
      "extracted ZIP app bundle must be Synara.app",
    );
  });

  it("mounts a DMG read-only, validates its payload, and always detaches it", () => {
    const commands: Array<{ readonly command: string; readonly args: ReadonlyArray<string> }> = [];
    const appChecks: string[] = [];
    const evidence = verifyMacDiskImagePayload("/release/Synara.dmg", "arm64", "TEAM123", {
      platform: "darwin",
      runCommand: (command, args) => {
        commands.push({ command, args });
        if (command === "hdiutil" && args[0] === "attach") {
          mkdirSync(join(args[5]!, "Synara.app"));
        }
        return successfulCommand();
      },
      verifyArchitectures: (appBundlePath, expectedArch) => {
        appChecks.push(`architecture:${expectedArch}:${appBundlePath}`);
        return [join(appBundlePath, "Contents", "MacOS", "Synara")];
      },
      verifyTeamIdentity: (appBundlePath, binaries) => {
        appChecks.push(`team:${binaries.length}:${appBundlePath}`);
        return "TEAM123";
      },
    });

    expect(evidence).toEqual({ appBundle: "Synara.app", teamId: "TEAM123" });
    expect(commands[0]).toMatchObject({
      command: "hdiutil",
      args: [
        "attach",
        "-readonly",
        "-nobrowse",
        "-noautoopen",
        "-mountpoint",
        expect.any(String),
        "/release/Synara.dmg",
      ],
    });
    expect(
      commands.some(({ command, args }) => command === "codesign" && args[1] === "--deep"),
    ).toBe(true);
    expect(commands.at(-1)).toMatchObject({
      command: "hdiutil",
      args: ["detach", expect.any(String)],
    });
    expect(appChecks).toHaveLength(2);
  });

  it("detaches the DMG when payload architecture validation fails", () => {
    const detachArguments: ReadonlyArray<string>[] = [];
    expect(() =>
      verifyMacDiskImagePayload("/release/Synara.dmg", "x64", "TEAM123", {
        platform: "darwin",
        runCommand: (command, args) => {
          if (command === "hdiutil" && args[0] === "attach") {
            mkdirSync(join(args[5]!, "Synara.app"));
          }
          if (command === "hdiutil" && args[0] === "detach") detachArguments.push(args);
          return successfulCommand();
        },
        verifyArchitectures: () => {
          throw new Error("mixed Mach-O architecture");
        },
      }),
    ).toThrow("mixed Mach-O architecture");
    expect(detachArguments).toHaveLength(1);
  });

  it("rejects a mismatched payload Team ID and force-detaches after a normal detach failure", () => {
    const detachArguments: ReadonlyArray<string>[] = [];
    expect(() =>
      verifyMacDiskImagePayload("/release/Synara.dmg", "arm64", "TEAM123", {
        platform: "darwin",
        runCommand: (command, args) => {
          if (command === "hdiutil" && args[0] === "attach") {
            mkdirSync(join(args[5]!, "Synara.app"));
          }
          if (command === "hdiutil" && args[0] === "detach") {
            detachArguments.push(args);
            if (args[1] !== "-force") throw new Error("resource busy");
          }
          return successfulCommand();
        },
        verifyArchitectures: (appBundlePath) => [join(appBundlePath, "Synara")],
        verifyTeamIdentity: () => "OTHERTEAM",
      }),
    ).toThrow("does not match expected TEAM123");
    expect(detachArguments.map((args) => args.slice(0, 2))).toEqual([
      ["detach", expect.any(String)],
      ["detach", "-force"],
    ]);
  });

  it("closes ZIP verification over expected architecture and every nested Mach-O Team ID", () => {
    const appBundlePath = "/extracted/Synara.app";
    const binaries = [
      `${appBundlePath}/Contents/MacOS/Synara`,
      `${appBundlePath}/Contents/Frameworks/Synara Helper`,
    ];
    const calls: string[] = [];
    const evidence = verifyMacZipApp(appBundlePath, "x64", "TEAM123", {
      verifyArchitectures: (path, expectedArch) => {
        calls.push(`architecture:${path}:${expectedArch}`);
        return binaries;
      },
      verifyTeamIdentity: (path, inspectedBinaries) => {
        calls.push(`team:${path}:${inspectedBinaries.join("|")}`);
        return "TEAM123";
      },
    });

    expect(calls).toEqual([
      `architecture:${appBundlePath}:x64`,
      `team:${appBundlePath}:${binaries.join("|")}`,
    ]);
    expect(evidence).toEqual({
      teamId: "TEAM123",
      checks: ["Mach-O architecture x64 in zip payload", "Team ID consistency in zip payload"],
    });
  });

  it("rejects a ZIP whose nested signing identity differs from the expected Team ID", () => {
    expect(() =>
      verifyMacZipApp("/extracted/Synara.app", "arm64", "TEAM123", {
        verifyArchitectures: () => ["/extracted/Synara.app/Contents/MacOS/Synara"],
        verifyTeamIdentity: () => "OTHERTEAM",
      }),
    ).toThrow("macOS ZIP app team ID OTHERTEAM does not match expected TEAM123");
  });
});
