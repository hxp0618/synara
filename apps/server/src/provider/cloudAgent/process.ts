import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";

import {
  createCloudAgentStdioClient,
  type CloudAgentStdioClient,
} from "@synara/cloud-agent-runtime";

import { buildProviderChildEnvironment } from "../../providerChildEnvironment.ts";
import type { CloudAgentBackendConfig } from "./config.ts";

export type CloudAgentProcessClient = CloudAgentStdioClient;
export type CloudAgentProcessClientFactory = (input: {
  readonly cwd: string;
}) => Promise<CloudAgentProcessClient>;

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
    const environment = buildProviderChildEnvironment({
      provider: "codex",
      ...(input.baseEnvironment ? { baseEnv: input.baseEnvironment } : {}),
    });
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
