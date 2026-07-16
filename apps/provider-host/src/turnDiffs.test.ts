import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import type { RunnerMessage } from "./providerHost";
import { summarizeUnifiedDiff, TurnDiffCollector } from "./turnDiffs";

describe("Turn Diff collector", () => {
  it("keeps a bounded Diff inline", async () => {
    const messages: RunnerMessage[] = [];
    const collector = new TurnDiffCollector({
      runtimeOutputDirectory: undefined,
      provider: "codex",
      emit: (message) => messages.push(message),
    });
    const diff = "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-old\n+new\n";

    collector.observeSnapshot(diff);
    await collector.flush();
    await collector.flush();

    expect(messages).toEqual([
      { type: "event", eventType: "turn.diff.updated", payload: { unifiedDiff: diff } },
    ]);
  });

  it("writes one large Diff beneath the bound Runtime Output Root", async () => {
    const runtimeOutputDirectory = mkdtempSync(join(tmpdir(), "synara-turn-diff-"));
    const messages: RunnerMessage[] = [];
    try {
      const collector = new TurnDiffCollector({
        runtimeOutputDirectory,
        provider: "claudeAgent",
        emit: (message) => messages.push(message),
      });
      const diff = [
        "diff --git a/large.txt b/large.txt",
        "--- a/large.txt",
        "+++ b/large.txt",
        "@@ -1,1 +1,5000 @@",
        "-before",
        ...Array.from({ length: 5_000 }, (_, index) => `+after-${index}-${"x".repeat(16)}`),
        "",
      ].join("\n");

      collector.observePatch("large.txt", diff);
      await collector.flush();

      expect(messages).toHaveLength(1);
      const artifact = messages[0];
      expect(artifact).toMatchObject({
        type: "artifact",
        artifact: {
          kind: "diff",
          originalName: "turn.diff",
          contentType: "text/x-diff; charset=utf-8",
          sourceRoot: "runtime-output",
          encoding: "utf-8",
          reportedSize: Buffer.byteLength(diff),
          fileCount: 1,
          additions: 5_000,
          deletions: 1,
        },
      });
      if (artifact?.type !== "artifact") throw new Error("Expected a Diff ArtifactCandidate.");
      expect(readFileSync(join(runtimeOutputDirectory, artifact.artifact.path), "utf8")).toBe(diff);
      expect(JSON.stringify(messages)).not.toContain(runtimeOutputDirectory);
    } finally {
      rmSync(runtimeOutputDirectory, { recursive: true, force: true });
    }
  });

  it("summarizes file and line counts without treating headers as edits", () => {
    expect(
      summarizeUnifiedDiff(
        "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1,2 @@\n-old\n+new\n+next\n",
      ),
    ).toEqual({ fileCount: 1, additions: 2, deletions: 1 });
  });
});
