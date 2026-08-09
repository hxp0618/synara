import { createHash } from "node:crypto";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import { verifyRuntimeDigest } from "./process.ts";

describe("verifyRuntimeDigest", () => {
  it("accepts exact runtime bytes and rejects drift", async () => {
    const directory = await mkdtemp(join(tmpdir(), "synara-cloud-agent-runtime-"));
    try {
      const runtimePath = join(directory, "runtime.mjs");
      await writeFile(runtimePath, "runtime-bytes");
      const digest = createHash("sha256").update("runtime-bytes").digest("hex");
      await expect(verifyRuntimeDigest(runtimePath, digest)).resolves.toBeUndefined();
      await expect(verifyRuntimeDigest(runtimePath, "0".repeat(64))).rejects.toThrow(
        "digest mismatch",
      );
    } finally {
      await rm(directory, { recursive: true });
    }
  });
});
