import { assert, describe, it } from "@effect/vitest";

import {
  createDesktopPlatformBuildConfig,
  DESKTOP_ENROLLMENT_PROTOCOLS,
  MAC_APPSNAP_HELPER_ASAR_EXCLUSION,
  MAC_APPSNAP_HELPER_BUNDLE_PATH,
  MAC_APPSNAP_HELPER_STAGE_PATH,
  MAC_ARM64_PREBUILD_EXCLUSION,
  MAC_ENTITLEMENTS_PATH,
  MAC_INHERITED_ENTITLEMENTS_PATH,
  MAC_X64_PREBUILD_EXCLUSION,
  MICROPHONE_USAGE_DESCRIPTION,
  NODE_PTY_ASAR_UNPACK_GLOBS,
  resolveDesktopArtifactName,
  resolveDesktopDependencyInstallTarget,
  validateDesktopNativeBuildHost,
  WINDOWS_INSTALLER_GUID,
} from "./lib/desktop-platform-build-config.ts";
import { BRAND_ASSET_PATHS } from "./lib/brand-assets.ts";

describe("createDesktopPlatformBuildConfig", () => {
  it("adds explicit microphone entitlements to macOS builds", () => {
    const config = createDesktopPlatformBuildConfig({
      platform: "mac",
      target: "dmg",
      arch: "arm64",
      signed: true,
    });
    const mac = config.mac as Record<string, unknown>;
    const dmg = config.dmg as Record<string, unknown>;
    const extendInfo = mac.extendInfo as Record<string, unknown>;

    assert.deepStrictEqual(mac.target, ["dmg", "zip"]);
    assert.equal(mac.icon, "icon.icns");
    assert.deepStrictEqual(config.asarUnpack, ["node_modules/node-pty/**"]);
    assert.deepStrictEqual(config.protocols, [
      { name: "Synara Desktop Enrollment", schemes: ["synara"] },
    ]);
    assert.deepStrictEqual(DESKTOP_ENROLLMENT_PROTOCOLS, [
      { name: "Synara Desktop Enrollment", schemes: ["synara"] },
    ]);
    assert.equal(mac.hardenedRuntime, true);
    assert.equal(mac.notarize, true);
    assert.equal(dmg.sign, true);
    assert.equal(dmg.writeUpdateInfo, false);
    assert.equal(mac.entitlements, MAC_ENTITLEMENTS_PATH);
    assert.equal(mac.entitlementsInherit, MAC_INHERITED_ENTITLEMENTS_PATH);
    assert.equal(MAC_APPSNAP_HELPER_BUNDLE_PATH, "Contents/Helpers/synara-appsnap-helper");
    assert.deepStrictEqual(mac.binaries, ["Contents/Helpers/synara-appsnap-helper"]);
    assert.equal(mac.x64ArchFiles, "Contents/Helpers/synara-appsnap-helper");
    assert.equal(
      MAC_APPSNAP_HELPER_STAGE_PATH,
      "apps/desktop/native/appsnap/build/synara-appsnap-helper",
    );
    assert.equal(MAC_APPSNAP_HELPER_ASAR_EXCLUSION, "!apps/desktop/native/appsnap/build/**");
    assert.deepStrictEqual(config.files, [
      "**/*",
      MAC_APPSNAP_HELPER_ASAR_EXCLUSION,
      MAC_X64_PREBUILD_EXCLUSION,
    ]);
    assert.deepStrictEqual(config.extraFiles, [
      {
        from: "apps/desktop/native/appsnap/build/synara-appsnap-helper",
        to: "Helpers/synara-appsnap-helper",
      },
    ]);
    assert.equal(extendInfo.NSMicrophoneUsageDescription, MICROPHONE_USAGE_DESCRIPTION);
    assert.equal(extendInfo.NSScreenCaptureUsageDescription, undefined);
  });

  it("leaves the DMG container unsigned for build-only macOS artifacts", () => {
    const config = createDesktopPlatformBuildConfig({
      platform: "mac",
      target: "dmg",
      signed: false,
    });

    assert.deepStrictEqual(config.dmg, { sign: false, writeUpdateInfo: false });
    assert.equal(
      resolveDesktopArtifactName({ platform: "mac", signed: false }),
      "Synara-${version}-${arch}-unsigned-build-only.${ext}",
    );
  });

  it("development-signs the whole local app without claiming distribution notarization", () => {
    const config = createDesktopPlatformBuildConfig({
      platform: "mac",
      target: "dmg",
      macDevelopmentIdentity: "Apple Development: Local Developer (LOCALTEAM)",
    });
    const mac = config.mac as Record<string, unknown>;

    assert.equal(mac.hardenedRuntime, true);
    assert.equal(mac.notarize, false);
    assert.equal(mac.identity, "Apple Development: Local Developer (LOCALTEAM)");
    assert.deepStrictEqual(config.dmg, { sign: false, writeUpdateInfo: false });
    assert.equal(
      resolveDesktopArtifactName({
        platform: "mac",
        macDevelopmentIdentity: "Apple Development: Local Developer (LOCALTEAM)",
      }),
      "Synara-${version}-${arch}.${ext}",
    );
  });

  it("keeps release and non-macOS artifact names stable", () => {
    assert.equal(
      resolveDesktopArtifactName({ platform: "mac", signed: true }),
      "Synara-${version}-${arch}.${ext}",
    );
    assert.equal(
      resolveDesktopArtifactName({ platform: "linux", signed: false }),
      "Synara-${version}-${arch}.${ext}",
    );
    assert.equal(
      resolveDesktopArtifactName({ platform: "win", signed: false }),
      "Synara-${version}-${arch}.${ext}",
    );
  });

  it("keeps only target-compatible macOS prebuild directories", () => {
    const x64 = createDesktopPlatformBuildConfig({
      platform: "mac",
      target: "zip",
      arch: "x64",
    });
    const universal = createDesktopPlatformBuildConfig({
      platform: "mac",
      target: "zip",
      arch: "universal",
    });

    assert.deepStrictEqual(x64.files, [
      "**/*",
      MAC_APPSNAP_HELPER_ASAR_EXCLUSION,
      MAC_ARM64_PREBUILD_EXCLUSION,
    ]);
    assert.deepStrictEqual(universal.files, ["**/*", MAC_APPSNAP_HELPER_ASAR_EXCLUSION]);
  });

  it("leaves non-macOS platform configs unchanged", () => {
    const linux = createDesktopPlatformBuildConfig({
      platform: "linux",
      target: "AppImage",
    });
    const win = createDesktopPlatformBuildConfig({
      platform: "win",
      target: "nsis",
      windowsAzureSignOptions: { publisherName: "Synara" },
    });

    assert.equal(linux.mac, undefined);
    assert.equal(linux.extraFiles, undefined);
    assert.deepStrictEqual(linux.asarUnpack, ["node_modules/node-pty/**"]);
    assert.deepStrictEqual(linux.protocols, [
      { name: "Synara Desktop Enrollment", schemes: ["synara"] },
    ]);
    assert.deepStrictEqual(linux.linux, {
      target: ["AppImage"],
      executableName: "synara",
      icon: "icon.png",
      category: "Development",
      desktop: {
        entry: {
          StartupWMClass: "synara",
        },
      },
    });

    assert.equal(win.mac, undefined);
    assert.equal(win.extraFiles, undefined);
    assert.deepStrictEqual(win.asarUnpack, ["node_modules/node-pty/**"]);
    assert.deepStrictEqual(win.protocols, [
      { name: "Synara Desktop Enrollment", schemes: ["synara"] },
    ]);
    assert.equal(WINDOWS_INSTALLER_GUID, "368107a8-afe6-5db5-ab3b-d4f331684868");
    assert.deepStrictEqual(win.nsis, {
      guid: WINDOWS_INSTALLER_GUID,
    });
    assert.deepStrictEqual(win.win, {
      target: ["nsis"],
      icon: "icon.ico",
      publisherName: "Synara",
      azureSignOptions: { publisherName: "Synara" },
    });
  });

  it("omits Azure signing options for unsigned build-only artifacts", () => {
    const config = createDesktopPlatformBuildConfig({
      platform: "win",
      target: "nsis",
    });

    assert.deepStrictEqual(config.win, {
      target: ["nsis"],
      icon: "icon.ico",
    });
  });

  it("keeps node-pty unpacked from ASAR in generated build config", () => {
    const config = createDesktopPlatformBuildConfig({
      platform: "linux",
      target: "AppImage",
    });

    assert.deepStrictEqual([...NODE_PTY_ASAR_UNPACK_GLOBS], ["node_modules/node-pty/**"]);
    assert.deepStrictEqual(config.asarUnpack, [...NODE_PTY_ASAR_UNPACK_GLOBS]);
  });

  it("installs native optional dependencies for the artifact target instead of the host", () => {
    assert.deepStrictEqual(
      resolveDesktopDependencyInstallTarget({ platform: "mac", arch: "x64" }),
      { os: "darwin", cpu: "x64" },
    );
    assert.deepStrictEqual(
      resolveDesktopDependencyInstallTarget({ platform: "mac", arch: "universal" }),
      { os: "darwin", cpu: "*" },
    );
    assert.deepStrictEqual(
      resolveDesktopDependencyInstallTarget({ platform: "win", arch: "arm64" }),
      { os: "win32", cpu: "arm64" },
    );
  });

  it("blocks unsupported or non-matching Linux native build hosts", () => {
    assert.equal(
      validateDesktopNativeBuildHost({
        platform: "linux",
        arch: "x64",
        hostPlatform: "linux",
        hostArch: "x64",
      }),
      null,
    );

    assert.equal(
      validateDesktopNativeBuildHost({
        platform: "linux",
        arch: "universal",
        hostPlatform: "linux",
        hostArch: "x64",
      }),
      "Linux desktop artifacts support x64 or arm64 builds, not universal builds.",
    );

    const issue = validateDesktopNativeBuildHost({
      platform: "linux",
      arch: "x64",
      hostPlatform: "darwin",
      hostArch: "arm64",
    });

    assert.ok(issue?.includes("Build linux/x64 on a matching Linux host"));
  });

  it("requires a macOS host for the native Swift AppSnap helper", () => {
    assert.equal(
      validateDesktopNativeBuildHost({
        platform: "mac",
        arch: "universal",
        hostPlatform: "darwin",
        hostArch: "arm64",
      }),
      null,
    );

    const issue = validateDesktopNativeBuildHost({
      platform: "mac",
      arch: "arm64",
      hostPlatform: "linux",
      hostArch: "arm64",
    });
    assert.ok(issue?.includes("Build mac/arm64 on macOS"));
  });

  it("keeps separate macOS sources for solid and rounded icons", () => {
    assert.equal(BRAND_ASSET_PATHS.productionMacIconPng, "assets/prod/black-macos-1024.png");
    assert.equal(
      BRAND_ASSET_PATHS.productionMacLegacyIconPng,
      "assets/prod/black-macos-legacy-1024.png",
    );
  });
});
