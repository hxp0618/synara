import { createHash } from "node:crypto";
import { spawn, type ChildProcess, type SpawnOptions } from "node:child_process";
import { existsSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";

import { createCloudAgentStdioClient } from "@synara/cloud-agent-distribution";

import { buildProviderChildEnvironment } from "../../providerChildEnvironment.ts";
import { BUNDLED_CLOUD_AGENT_RUNTIME_SHA256 } from "../../../../../scripts/lib/cloudAgentCandidate.ts";
import type { CloudAgentBackendConfig } from "./config.ts";

type PublicCloudAgentProcessClient = ReturnType<typeof createCloudAgentStdioClient>;

export type CloudAgentProcessTermination =
  | {
      readonly kind: "exit";
      readonly code: number | null;
      readonly signal: NodeJS.Signals | null;
    }
  | { readonly kind: "error"; readonly error: Error };

export interface CloudAgentProcessClient extends PublicCloudAgentProcessClient {
  readonly exit: Promise<CloudAgentProcessTermination>;
}

export type CloudAgentProcessClientFactory = (input: {
  readonly cwd: string;
}) => Promise<CloudAgentProcessClient>;

export function buildCloudAgentProcessEnvironment(
  baseEnvironment?: NodeJS.ProcessEnv,
): NodeJS.ProcessEnv {
  return buildProviderChildEnvironment({
    provider: "codex",
    ...(baseEnvironment ? { baseEnv: baseEnvironment } : {}),
    // A packaged desktop server is itself running through Electron in Node mode.
    // Its process.execPath is the Electron binary, so the runtime child needs the
    // same narrowly granted flag to execute the bundled standalone module.
    inheritedNativeCapabilityKeys: ["ELECTRON_RUN_AS_NODE"],
  });
}

export function resolveDefaultCloudAgentRuntimePath(entrypoint = process.argv[1]): string {
  const moduleDirectory = entrypoint ? dirname(resolve(entrypoint)) : process.cwd();
  const bundledRuntime = resolve(moduleDirectory, "cloudAgentRuntimeChild.mjs");
  if (existsSync(bundledRuntime)) return bundledRuntime;
  throw new Error(
    `Bundled Cloud Agent Runtime is missing beside the server entrypoint: ${bundledRuntime}.`,
  );
}

export function makeCloudAgentProcessClientFactory(input: {
  readonly config: CloudAgentBackendConfig;
  readonly bundledRuntimePath?: string;
  readonly baseEnvironment?: NodeJS.ProcessEnv;
  readonly spawnProcess?: typeof spawn;
}): CloudAgentProcessClientFactory {
  const customRuntime = input.config.runtimePath !== undefined;
  const runtimePath = input.config.runtimePath ?? input.bundledRuntimePath;
  if (!runtimePath) {
    throw new Error("Bundled Cloud Agent Runtime path is required.");
  }
  const runtimeSha256 = customRuntime
    ? input.config.runtimeSha256
    : BUNDLED_CLOUD_AGENT_RUNTIME_SHA256;
  if (!runtimeSha256) {
    throw new Error("A custom Cloud Agent Runtime requires an explicit SHA-256 digest.");
  }

  return async ({ cwd }) => {
    // Keep verification immediately adjacent to the public client's synchronous
    // spawn. This deliberately re-reads the bytes for every per-thread child.
    await verifyRuntimeDigest(runtimePath, runtimeSha256);
    const environment = buildCloudAgentProcessEnvironment(input.baseEnvironment);
    let resolveExit!: (termination: CloudAgentProcessTermination) => void;
    let settled = false;
    const exit = new Promise<CloudAgentProcessTermination>((resolve) => {
      resolveExit = resolve;
    });
    const observe = (termination: CloudAgentProcessTermination) => {
      if (settled) return;
      settled = true;
      resolveExit(termination);
    };
    const spawnProcess = ((
      command: string,
      args?: readonly string[],
      options?: SpawnOptions,
    ): ChildProcess => {
      const child = Reflect.apply(input.spawnProcess ?? spawn, undefined, [
        command,
        args,
        options,
      ]) as ChildProcess;
      child.once("exit", (code, signal) => observe({ kind: "exit", code, signal }));
      child.once("error", (error) => observe({ kind: "error", error }));
      return child;
    }) as typeof spawn;
    const client = createCloudAgentStdioClient({
      command: process.execPath,
      args: [runtimePath],
      cwd,
      environment,
      extendEnvironment: false,
      spawnProcess,
    });
    return Object.freeze({
      get pid() {
        return client.pid;
      },
      execute: client.execute,
      subscribe: client.subscribe,
      close: client.close,
      exit,
    });
  };
}

export async function verifyRuntimeDigest(
  runtimePath: string,
  expectedSha256: string,
): Promise<void> {
  const actual = createHash("sha256")
    .update(await readFile(runtimePath))
    .digest("hex");
  if (actual !== expectedSha256) {
    throw new Error(
      `Cloud Agent runtime digest mismatch for ${runtimePath}: expected ${expectedSha256}, got ${actual}.`,
    );
  }
}
