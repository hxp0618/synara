import * as FS from "node:fs";
import * as OS from "node:os";
import * as Path from "node:path";

import { afterEach, describe, expect, it } from "vitest";

import {
  parseDesktopConnectionMode,
  planDesktopConnectionStartup,
  readDesktopConnectionMode,
  resolveInitialDesktopConnectionMode,
  writeDesktopConnectionMode,
} from "./desktopConnectionMode";

const temporaryDirectories: string[] = [];

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) {
    FS.rmSync(directory, { recursive: true, force: true });
  }
});

describe("Desktop connection mode", () => {
  it("requires an explicit choice whenever no mode has been persisted", () => {
    expect(planDesktopConnectionStartup({ persisted: null })).toEqual({
      mode: "unselected",
      requiresSelection: true,
      initializeCloudConnection: false,
    });
  });

  it("restores the persisted local choice without initializing Cloud Panel", () => {
    expect(
      planDesktopConnectionStartup({
        persisted: { version: 1, mode: "local", updatedAt: "2026-08-01T00:00:00.000Z" },
      }),
    ).toEqual({
      mode: "local",
      requiresSelection: false,
      initializeCloudConnection: false,
    });
  });

  it("restores and initializes the persisted Cloud Panel choice on later launches", () => {
    expect(
      planDesktopConnectionStartup({
        persisted: { version: 1, mode: "cloud", updatedAt: "2026-08-01T00:00:00.000Z" },
      }),
    ).toEqual({
      mode: "cloud",
      requiresSelection: false,
      initializeCloudConnection: true,
    });
  });

  it("atomically persists and restores the last explicit choice", () => {
    const directory = FS.mkdtempSync(Path.join(OS.tmpdir(), "synara-desktop-mode-"));
    temporaryDirectories.push(directory);
    const filePath = Path.join(directory, "desktop-connection-mode.json");

    writeDesktopConnectionMode(filePath, "cloud", new Date("2026-08-01T00:00:00Z"));
    expect(readDesktopConnectionMode(filePath)).toEqual({
      version: 1,
      mode: "cloud",
      updatedAt: "2026-08-01T00:00:00.000Z",
    });
    expect(FS.statSync(filePath).mode & 0o777).toBe(0o600);

    writeDesktopConnectionMode(filePath, "local", new Date("2026-08-01T00:01:00Z"));
    expect(readDesktopConnectionMode(filePath)?.mode).toBe("local");
  });

  it("fails closed to unselected for malformed state", () => {
    expect(
      parseDesktopConnectionMode({ version: 1, mode: "remote", updatedAt: "today" }),
    ).toBeNull();
  });

  it("treats a damaged persisted file as a first-use choice", () => {
    const directory = FS.mkdtempSync(Path.join(OS.tmpdir(), "synara-desktop-mode-"));
    temporaryDirectories.push(directory);
    const filePath = Path.join(directory, "desktop-connection-mode.json");
    FS.writeFileSync(filePath, "not-json");

    expect(
      resolveInitialDesktopConnectionMode({
        persisted: readDesktopConnectionMode(filePath),
      }),
    ).toBe("unselected");
  });
});
