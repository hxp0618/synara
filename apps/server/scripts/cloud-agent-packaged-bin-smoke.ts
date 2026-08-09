import { join } from "node:path";

import { createCloudAgentStdioClient } from "@synara/cloud-agent-distribution";
import {
  CLOUD_AGENT_PROTOCOL_VERSION,
  assertCloudAgentCommandEnvelope,
  assertCloudAgentMessageEnvelope,
  type CloudAgentCommandEnvelope,
} from "@synara/cloud-agent-protocol";

import { assertCloudAgentNode24 } from "../src/provider/cloudAgent/config.ts";
import {
  assertCloudAgentRuntimeAsset,
  CLOUD_AGENT_RUNTIME_ASSET_NAME,
  readCloudAgentCandidateLock,
} from "./cloudAgentRuntimeAsset.ts";

const serverDirectory = join(import.meta.dirname, "..");
const repoRoot = join(serverDirectory, "../..");
const assetPath = join(serverDirectory, "dist", CLOUD_AGENT_RUNTIME_ASSET_NAME);

assertCloudAgentNode24();
const candidateLock = readCloudAgentCandidateLock(repoRoot, { required: true })!;
assertCloudAgentRuntimeAsset({
  assetPath,
  expectedSha256: candidateLock.standaloneRuntime.sha256,
});

const command: CloudAgentCommandEnvelope = {
  requestId: "synara-packaged-bin-smoke",
  protocolVersion: CLOUD_AGENT_PROTOCOL_VERSION,
  executionId: "synara-packaged-bin-smoke",
  generation: 1,
  commandType: "Describe",
  commandId: "synara-packaged-bin-smoke",
  occurredAt: new Date().toISOString(),
  payload: { provider: "codex" },
};
assertCloudAgentCommandEnvelope(command);

const client = createCloudAgentStdioClient({
  command: process.execPath,
  args: [assetPath],
  cwd: repoRoot,
  environment: {
    PATH: process.env.PATH,
    HOME: process.env.HOME,
    TMPDIR: process.env.TMPDIR,
  },
  extendEnvironment: false,
});

try {
  const terminal = await client.execute(command);
  assertCloudAgentMessageEnvelope(terminal);
  if (
    terminal.requestId !== command.requestId ||
    terminal.executionId !== command.executionId ||
    terminal.commandId !== command.commandId ||
    terminal.generation !== command.generation
  ) {
    throw new Error("Packaged Cloud Agent Runtime lost Protocol correlation metadata.");
  }
  process.stdout.write(
    `Cloud Agent packaged-bin smoke passed with terminal ${terminal.messageType}.\n`,
  );
} finally {
  await client.close();
}
