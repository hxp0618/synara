import { createHash } from "node:crypto";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";

import type { RunnerMessage } from "./providerHost";

const DIFF_CONTENT_TYPE = "text/x-diff; charset=utf-8";
const INLINE_DIFF_MAX_BYTES = 48 * 1024;
const DIFF_ARTIFACT_MAX_BYTES = 16 * 1024 * 1024;
const DIFF_OUTPUT_DIRECTORY = "provider-diffs";

type TurnDiffCollectorOptions = {
  runtimeOutputDirectory: string | undefined;
  provider: string;
  emit: (message: RunnerMessage) => void;
};

type DiffSummary = {
  fileCount: number;
  additions: number;
  deletions: number;
};

export class TurnDiffCollector {
  private snapshot: string | undefined;
  private readonly patches = new Map<string, string>();
  private flushed = false;

  constructor(private readonly options: TurnDiffCollectorOptions) {}

  observeSnapshot(candidate: unknown): void {
    if (typeof candidate !== "string") return;
    this.snapshot = candidate;
  }

  observePatch(key: unknown, candidate: unknown): void {
    if (typeof key !== "string" || key.trim() === "" || typeof candidate !== "string") return;
    if (candidate === "") {
      this.patches.delete(key);
      return;
    }
    this.patches.set(key, candidate);
  }

  async flush(): Promise<void> {
    if (this.flushed) return;
    this.flushed = true;

    const unifiedDiff = this.diff();
    if (unifiedDiff === "") return;
    const sizeBytes = Buffer.byteLength(unifiedDiff);
    if (sizeBytes <= INLINE_DIFF_MAX_BYTES) {
      this.options.emit({
        type: "event",
        eventType: "turn.diff.updated",
        payload: { unifiedDiff },
      });
      return;
    }

    if (sizeBytes > DIFF_ARTIFACT_MAX_BYTES) {
      this.warn(
        `Provider reported a ${sizeBytes}-byte Turn Diff, above Synara's ${DIFF_ARTIFACT_MAX_BYTES}-byte standalone Diff Artifact limit; the Workspace Checkpoint remains authoritative.`,
      );
      return;
    }

    const runtimeOutputDirectory = this.options.runtimeOutputDirectory;
    if (!runtimeOutputDirectory) {
      this.warn(
        "Provider reported a large Turn Diff, but this Runner has no bound Runtime Output Root for a safe Diff Artifact.",
      );
      return;
    }

    const sha256 = createHash("sha256").update(unifiedDiff).digest("hex");
    const relativePath = join(DIFF_OUTPUT_DIRECTORY, `${sha256}.diff`);
    const outputDirectory = join(runtimeOutputDirectory, DIFF_OUTPUT_DIRECTORY);
    await mkdir(outputDirectory, { recursive: true, mode: 0o700 });
    await writeFile(join(runtimeOutputDirectory, relativePath), unifiedDiff, {
      encoding: "utf8",
      mode: 0o600,
    });
    const summary = summarizeUnifiedDiff(unifiedDiff);
    this.options.emit({
      type: "artifact",
      artifact: {
        path: relativePath,
        kind: "diff",
        originalName: "turn.diff",
        contentType: DIFF_CONTENT_TYPE,
        sourceRoot: "runtime-output",
        encoding: "utf-8",
        reportedSize: sizeBytes,
        ...summary,
      },
    });
  }

  private diff(): string {
    if (this.snapshot !== undefined) return this.snapshot;
    return [...this.patches.values()].join("\n");
  }

  private warn(message: string): void {
    this.options.emit({
      type: "event",
      eventType: "runtime.provider.warning",
      payload: { provider: this.options.provider, message },
    });
  }
}

export function summarizeUnifiedDiff(unifiedDiff: string): DiffSummary {
  const files = new Set<string>();
  let additions = 0;
  let deletions = 0;
  for (const line of unifiedDiff.split("\n")) {
    if (line.startsWith("diff --git ")) {
      const match = /^diff --git (?:"?a\/(.+?)"?) (?:"?b\/(.+?)"?)$/u.exec(line);
      files.add(match?.[2] ?? line);
      continue;
    }
    if (line.startsWith("+") && !line.startsWith("+++")) additions += 1;
    if (line.startsWith("-") && !line.startsWith("---")) deletions += 1;
  }
  return { fileCount: files.size, additions, deletions };
}
