import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { dirname, resolve } from "node:path";

import {
  createCloudAgentStdioClient,
  resolveCloudAgentRuntimeExecutable,
} from "@synara/cloud-agent-distribution";

import { buildProviderChildEnvironment } from "../../providerChildEnvironment.ts";
import type { CloudAgentBackendConfig } from "./config.ts";

export type CloudAgentProcessClient = ReturnType<typeof createCloudAgentStdioClient>;
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
  const manifestPath = createRequire(resolve(moduleDirectory, "package.json")).resolve(
    "@synara/cloud-agent-distribution/manifest.json",
  );
  return resolveCloudAgentRuntimeExecutable(dirname(manifestPath));
}

export function makeCloudAgentProcessClientFactory(input: {
  readonly config: CloudAgentBackendConfig;
  readonly bundledRuntimePath: string;
  readonly baseEnvironment?: NodeJS.ProcessEnv;
}): CloudAgentProcessClientFactory {
  const runtimePath = input.config.runtimePath ?? input.bundledRuntimePath;
  let verified: Promise<void> | undefined;
  const verify = () =>
    (verified ??= verifyRuntimeDigest(runtimePath, input.config.runtimeSha256).catch((cause) => {
      verified = undefined;
      throw cause;
    }));

  return async ({ cwd }) => {
    await verify();
    const environment = buildCloudAgentProcessEnvironment(input.baseEnvironment);
    return createCloudAgentStdioClient({
      command: process.execPath,
      args: [runtimePath],
      cwd,
      environment,
      extendEnvironment: false,
    });
  };
}

export async function verifyRuntimeDigest(
  runtimePath: string,
  expectedSha256: string | undefined,
): Promise<void> {
  if (!expectedSha256) return;
  const actual = createHash("sha256")
    .update(await readFile(runtimePath))
    .digest("hex");
  if (actual !== expectedSha256) {
    throw new Error(
      `Cloud Agent runtime digest mismatch for ${runtimePath}: expected ${expectedSha256}, got ${actual}.`,
    );
  }
}
