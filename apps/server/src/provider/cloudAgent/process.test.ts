import { createHash } from "node:crypto";
import { spawn } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import {
  buildCloudAgentProcessEnvironment,
  makeCloudAgentProcessClientFactory,
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

  it("fails closed when the child bundled beside the server entrypoint is missing", async () => {
    const directory = await mkdtemp(join(tmpdir(), "synara-cloud-agent-missing-"));
    try {
      expect(() => resolveDefaultCloudAgentRuntimePath(join(directory, "index.mjs"))).toThrow(
        "Bundled Cloud Agent Runtime is missing",
      );
    } finally {
      await rm(directory, { recursive: true });
    }
  });

  it("re-hashes immediately before every spawn and rejects replaced bytes", async () => {
    const directory = await mkdtemp(join(tmpdir(), "synara-cloud-agent-respawn-"));
    try {
      const runtimePath = join(directory, "runtime.mjs");
      const runtime = "setInterval(() => {}, 1_000);\n";
      await writeFile(runtimePath, runtime);
      const digest = createHash("sha256").update(runtime).digest("hex");
      let spawnCount = 0;
      const factory = makeCloudAgentProcessClientFactory({
        config: { backend: "cloud-agent", runtimePath, runtimeSha256: digest },
        spawnProcess: ((...args: Parameters<typeof spawn>) => {
          spawnCount += 1;
          return Reflect.apply(spawn, undefined, args);
        }) as typeof spawn,
      });

      const first = await factory({ cwd: directory });
      expect(spawnCount).toBe(1);
      await first.close();
      await writeFile(runtimePath, "replaced-runtime-bytes\n");
      await expect(factory({ cwd: directory })).rejects.toThrow("digest mismatch");
      expect(spawnCount).toBe(1);
    } finally {
      await rm(directory, { recursive: true });
    }
  });

  it("exposes the public client's child termination without replacing its transport", async () => {
    const directory = await mkdtemp(join(tmpdir(), "synara-cloud-agent-exit-"));
    try {
      const runtimePath = join(directory, "runtime.mjs");
      const runtime = "process.exit(7);\n";
      await writeFile(runtimePath, runtime);
      const digest = createHash("sha256").update(runtime).digest("hex");
      const factory = makeCloudAgentProcessClientFactory({
        config: { backend: "cloud-agent", runtimePath, runtimeSha256: digest },
      });
      const client = await factory({ cwd: directory });
      await expect(client.exit).resolves.toEqual({ kind: "exit", code: 7, signal: null });
      await client.close();
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
