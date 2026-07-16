import { realpathSync } from "node:fs";
import { lstat } from "node:fs/promises";
import { isAbsolute, join, relative, resolve, sep } from "node:path";

import type { RunnerMessage } from "./providerHost";

const GENERATED_FILE_CONTENT_TYPE = "application/octet-stream";
const MAX_GENERATED_FILE_PATH_BYTES = 4 * 1024;
const MAX_TRACKED_GENERATED_FILES = 256;
const MAX_EMITTED_GENERATED_FILES = 64;
const VCS_METADATA_SEGMENTS = new Set([".git", ".hg", ".svn"]);

type WorkspaceGeneratedFileCollectorOptions = {
  workspaceDirectory: string;
  provider: string;
  emit: (message: RunnerMessage) => void;
};

export class WorkspaceGeneratedFileCollector {
  private readonly workspaceDirectory: string;
  private readonly workspaceDirectoryAliases: ReadonlyArray<string>;
  private readonly candidates = new Set<string>();
  private overflowed = false;
  private flushed = false;

  constructor(private readonly options: WorkspaceGeneratedFileCollectorOptions) {
    const configuredDirectory = resolve(options.workspaceDirectory);
    let canonicalDirectory = configuredDirectory;
    try {
      canonicalDirectory = realpathSync(configuredDirectory);
    } catch {
      // Agentd owns the authoritative Workspace open; missing roots simply
      // produce no standalone candidates in this Provider Host process.
    }
    this.workspaceDirectory = canonicalDirectory;
    this.workspaceDirectoryAliases = [...new Set([configuredDirectory, canonicalDirectory])];
  }

  observe(candidate: unknown): void {
    const relativePath = this.relativePath(candidate);
    if (!relativePath || this.candidates.has(relativePath)) return;
    if (this.candidates.size >= MAX_TRACKED_GENERATED_FILES) {
      this.overflowed = true;
      return;
    }
    this.candidates.add(relativePath);
  }

  remove(candidate: unknown): void {
    const relativePath = this.relativePath(candidate);
    if (relativePath) this.candidates.delete(relativePath);
  }

  async flush(): Promise<void> {
    if (this.flushed) return;
    this.flushed = true;

    const safeCandidates: string[] = [];
    for (const relativePath of [...this.candidates].sort()) {
      if (await isSafeRegularWorkspaceFile(this.workspaceDirectory, relativePath)) {
        safeCandidates.push(relativePath);
      }
    }

    const emittedCandidates = safeCandidates.slice(0, MAX_EMITTED_GENERATED_FILES);
    for (const path of emittedCandidates) {
      this.options.emit({
        type: "artifact",
        artifact: {
          path,
          kind: "generated_file",
          contentType: GENERATED_FILE_CONTENT_TYPE,
          sourceRoot: "workspace",
        },
      });
    }

    if (this.overflowed || safeCandidates.length > emittedCandidates.length) {
      this.options.emit({
        type: "event",
        eventType: "runtime.provider.warning",
        payload: {
          provider: this.options.provider,
          message:
            `Provider reported more than ${MAX_EMITTED_GENERATED_FILES} durable Workspace file changes; ` +
            "Synara limited standalone generated-file Artifacts while preserving the complete Workspace through its Checkpoint.",
        },
      });
    }
  }

  private relativePath(candidate: unknown): string | undefined {
    for (const workspaceDirectory of this.workspaceDirectoryAliases) {
      const relativePath = workspaceGeneratedFileRelativePath(workspaceDirectory, candidate);
      if (relativePath) return relativePath;
    }
    return undefined;
  }
}

export function workspaceGeneratedFileRelativePath(
  workspaceDirectory: string,
  candidate: unknown,
): string | undefined {
  if (
    typeof candidate !== "string" ||
    candidate.trim() === "" ||
    Buffer.byteLength(candidate) > MAX_GENERATED_FILE_PATH_BYTES ||
    /[\u0000-\u001f\u007f]/u.test(candidate)
  ) {
    return undefined;
  }

  let relativePath: string;
  try {
    const workspace = resolve(workspaceDirectory);
    const absoluteCandidate = isAbsolute(candidate)
      ? resolve(candidate)
      : resolve(workspace, candidate);
    relativePath = relative(workspace, absoluteCandidate);
  } catch {
    return undefined;
  }

  if (
    relativePath === "" ||
    relativePath === "." ||
    relativePath === ".." ||
    isAbsolute(relativePath) ||
    relativePath.startsWith(`..${sep}`) ||
    Buffer.byteLength(relativePath) > MAX_GENERATED_FILE_PATH_BYTES
  ) {
    return undefined;
  }
  if (relativePath.split(sep).some((segment) => VCS_METADATA_SEGMENTS.has(segment.toLowerCase()))) {
    return undefined;
  }
  return relativePath;
}

async function isSafeRegularWorkspaceFile(
  workspaceDirectory: string,
  relativePath: string,
): Promise<boolean> {
  const segments = relativePath.split(sep);
  let current = workspaceDirectory;
  for (const [index, segment] of segments.entries()) {
    current = join(current, segment);
    let info;
    try {
      info = await lstat(current);
    } catch {
      return false;
    }
    if (info.isSymbolicLink()) return false;
    if (index === segments.length - 1) return info.isFile();
    if (!info.isDirectory()) return false;
  }
  return false;
}
