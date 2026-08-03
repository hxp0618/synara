// FILE: desktopConnectionMode.ts
// Purpose: Persists the user's explicit local-versus-Cloud Panel Desktop choice.
// Layer: Desktop main-process state

import * as Crypto from "node:crypto";
import * as FS from "node:fs";
import * as Path from "node:path";

import type { DesktopConnectionMode } from "@synara/contracts";

export type PersistedDesktopConnectionMode = {
  readonly version: 1;
  readonly mode: Exclude<DesktopConnectionMode, "unselected">;
  readonly updatedAt: string;
};

export type DesktopConnectionStartupPlan = {
  readonly mode: DesktopConnectionMode;
  readonly requiresSelection: boolean;
  readonly initializeCloudConnection: boolean;
};

export function parseDesktopConnectionMode(value: unknown): PersistedDesktopConnectionMode | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  if (
    record.version !== 1 ||
    (record.mode !== "local" && record.mode !== "cloud") ||
    typeof record.updatedAt !== "string" ||
    !record.updatedAt.endsWith("Z") ||
    !Number.isFinite(Date.parse(record.updatedAt))
  ) {
    return null;
  }
  return { version: 1, mode: record.mode, updatedAt: record.updatedAt };
}

export function readDesktopConnectionMode(filePath: string): PersistedDesktopConnectionMode | null {
  try {
    return parseDesktopConnectionMode(JSON.parse(FS.readFileSync(filePath, "utf8")));
  } catch {
    return null;
  }
}

export function writeDesktopConnectionMode(
  filePath: string,
  mode: Exclude<DesktopConnectionMode, "unselected">,
  now: Date = new Date(),
): PersistedDesktopConnectionMode {
  const state: PersistedDesktopConnectionMode = {
    version: 1,
    mode,
    updatedAt: now.toISOString(),
  };
  const directory = Path.dirname(filePath);
  FS.mkdirSync(directory, { recursive: true, mode: 0o700 });
  const temporaryPath = Path.join(
    directory,
    `.${Path.basename(filePath)}.${process.pid}.${Crypto.randomBytes(6).toString("hex")}.tmp`,
  );
  try {
    FS.writeFileSync(temporaryPath, `${JSON.stringify(state, null, 2)}\n`, {
      encoding: "utf8",
      flag: "wx",
      mode: 0o600,
    });
    FS.renameSync(temporaryPath, filePath);
    try {
      FS.chmodSync(filePath, 0o600);
    } catch {
      // Windows ACLs are authoritative; the atomic file content is already complete.
    }
  } finally {
    try {
      FS.rmSync(temporaryPath, { force: true });
    } catch {
      // A successful rename removes the temporary name; failed cleanup must not corrupt the target.
    }
  }
  return state;
}

export function resolveInitialDesktopConnectionMode(input: {
  readonly persisted: PersistedDesktopConnectionMode | null;
}): DesktopConnectionMode {
  if (input.persisted) return input.persisted.mode;
  return "unselected";
}

export function planDesktopConnectionStartup(input: {
  readonly persisted: PersistedDesktopConnectionMode | null;
}): DesktopConnectionStartupPlan {
  const mode = resolveInitialDesktopConnectionMode(input);
  return {
    mode,
    requiresSelection: mode === "unselected",
    initializeCloudConnection: shouldInitializeDesktopCloudConnection(mode),
  };
}

export function shouldInitializeDesktopCloudConnection(mode: DesktopConnectionMode): boolean {
  return mode === "cloud";
}
