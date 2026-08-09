import { createHash } from "node:crypto";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import {
  buildCloudAgentProcessEnvironment,
  resolveDefaultCloudAgentRuntimePath,
  verifyRuntimeDigest,
} from "./process.ts";

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

  it("prefers the child bundled beside the server entrypoint", async () => {
    const directory = await mkdtemp(join(tmpdir(), "synara-cloud-agent-bundled-"));
    try {
      const entrypoint = join(directory, "index.mjs");
      const runtimePath = join(directory, "cloudAgentRuntimeChild.mjs");
      await writeFile(entrypoint, "");
      await writeFile(runtimePath, "#!/usr/bin/env node\n");
      expect(resolveDefaultCloudAgentRuntimePath(entrypoint)).toBe(runtimePath);
    } finally {
      await rm(directory, { recursive: true });
    }
  });

  it("keeps Electron Node mode for the packaged runtime child only", () => {
    expect(
      buildCloudAgentProcessEnvironment({
        PATH: "/usr/bin",
        ELECTRON_RUN_AS_NODE: "1",
        NODE_OPTIONS: "--require=/tmp/inject.cjs",
        SYNARA_AUTH_TOKEN: "server-authority",
      }),
    ).toEqual({
      PATH: "/usr/bin",
      ELECTRON_RUN_AS_NODE: "1",
    });
  });
});
