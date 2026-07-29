// FILE: codexIsolatedHome.ts
// Purpose: Maintains the non-executable filesystem boundary for local Codex app-server homes.
// Layer: Server runtime utility

import { constants as fsConstants } from "node:fs";
import * as fs from "node:fs/promises";
import path from "node:path";

export const ISOLATED_CODEX_HOME_SHARED_SOURCE_ENTRIES: ReadonlySet<string> = new Set([
  "auth.json",
]);

const EXECUTABLE_CONFIG_ENTRIES = [
  ".env",
  "AGENTS.md",
  "agents",
  "hooks",
  "plugins",
  "rules",
  "shell_snapshots",
  "skills",
  "vendor_imports",
] as const;

// An early Stage 5 prototype linked these entries to the user's source home.
// Remove only those legacy symlinks; preserve real files created by an active
// isolated Codex store so concurrent app-server launches do not delete state.
const LEGACY_SHARED_STATE_ENTRIES = [
  "archived_sessions",
  "attachments",
  "generated_images",
  "history.jsonl",
  "installation_id",
  "models_cache.json",
  "session_index.jsonl",
  "sessions",
  "version.json",
] as const;

const MAX_SESSION_IMPORT_ENTRIES = 10_000;
const MAX_SESSION_IMPORT_DEPTH = 4;

async function removeSymlinkIfPresent(entryPath: string): Promise<void> {
  const stat = await fs.lstat(entryPath).catch((cause: unknown) => {
    if ((cause as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw cause;
  });
  if (stat?.isSymbolicLink()) {
    await fs.rm(entryPath, { force: true });
  }
}

export async function prepareIsolatedCodexHomeDirectories(overlayHomePath: string): Promise<void> {
  await Promise.all(
    EXECUTABLE_CONFIG_ENTRIES.map((entry) =>
      fs.rm(path.join(overlayHomePath, entry), { recursive: true, force: true }),
    ),
  );
  await Promise.all(
    LEGACY_SHARED_STATE_ENTRIES.map((entry) =>
      removeSymlinkIfPresent(path.join(overlayHomePath, entry)),
    ),
  );
  await fs.mkdir(path.join(overlayHomePath, "sessions"), { recursive: true });
}

function isSafeSessionThreadId(threadId: string): boolean {
  return /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/u.test(threadId);
}

async function findSessionFile(rootPath: string, threadId: string): Promise<string | undefined> {
  const stack: Array<{ readonly directoryPath: string; readonly depth: number }> = [
    { directoryPath: rootPath, depth: 0 },
  ];
  let observedEntries = 0;
  while (stack.length > 0) {
    const current = stack.pop();
    if (!current) break;
    const entries = await fs
      .readdir(current.directoryPath, { withFileTypes: true })
      .catch((cause: unknown) => {
        if ((cause as NodeJS.ErrnoException).code === "ENOENT") return [];
        throw cause;
      });
    observedEntries += entries.length;
    if (observedEntries > MAX_SESSION_IMPORT_ENTRIES) {
      throw new Error("Codex session import exceeded its bounded source-home scan.");
    }
    for (const entry of entries) {
      if (entry.isSymbolicLink()) continue;
      const entryPath = path.join(current.directoryPath, entry.name);
      if (
        entry.isFile() &&
        (entry.name === `${threadId}.jsonl` || entry.name.endsWith(`-${threadId}.jsonl`))
      ) {
        return entryPath;
      }
      if (entry.isDirectory() && current.depth < MAX_SESSION_IMPORT_DEPTH) {
        stack.push({ directoryPath: entryPath, depth: current.depth + 1 });
      }
    }
  }
  return undefined;
}

export async function importCodexSessionFiles(input: {
  readonly sourceHomePath: string;
  readonly overlayHomePath: string;
  readonly threadIds: ReadonlyArray<string>;
}): Promise<void> {
  for (const threadId of [...new Set(input.threadIds.map((value) => value.trim()))]) {
    if (!isSafeSessionThreadId(threadId)) {
      throw new Error("Codex session import received an invalid thread id.");
    }
    let sourcePath: string | undefined;
    let sourceRoot: string | undefined;
    for (const rootName of ["sessions", "archived_sessions"] as const) {
      const candidateRoot = path.join(input.sourceHomePath, rootName);
      const candidate = await findSessionFile(candidateRoot, threadId);
      if (candidate) {
        sourcePath = candidate;
        sourceRoot = candidateRoot;
        break;
      }
    }
    if (!sourcePath || !sourceRoot) continue;

    const relativePath = path.relative(sourceRoot, sourcePath);
    const targetPath = path.join(input.overlayHomePath, "sessions", relativePath);
    await fs.mkdir(path.dirname(targetPath), { recursive: true });
    await fs
      .copyFile(sourcePath, targetPath, fsConstants.COPYFILE_EXCL | fsConstants.COPYFILE_FICLONE)
      .catch((cause: unknown) => {
        if ((cause as NodeJS.ErrnoException).code !== "EEXIST") throw cause;
      });
    await fs.chmod(targetPath, 0o600).catch(() => undefined);
  }
}
