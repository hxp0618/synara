import { readFile, writeFile } from "node:fs/promises";
import { resolve } from "node:path";

import { environment, outputText, stableKey, streamUntilTerminal } from "./common";

type RepositoryJob = { id: string; projectId: string; instruction: string };
type Checkpoint = {
  sessionId?: string;
  sequence: number;
  status: "pending" | "running" | "completed" | "failed";
  error?: string;
};
type Checkpoints = Record<string, Checkpoint>;

const inputPath = resolve(process.argv[2] ?? "repositories.json");
const checkpointPath = resolve(
  process.env.POLARIS_CHECKPOINT_FILE?.trim() || ".polaris-migration-checkpoints.json",
);
const jobs = parseJobs(JSON.parse(await readFile(inputPath, "utf8")) as unknown);
const checkpoints = await loadCheckpoints(checkpointPath);
const { polaris } = environment();

for (const job of jobs) {
  const previous = checkpoints[job.id];
  if (previous?.status === "completed") {
    console.log(`[skip] ${job.id}`);
    continue;
  }
  try {
    const session = await polaris.sessions.create(
      {
        projectId: job.projectId,
        title: `Batch migration: ${job.id}`,
        provider: process.env.POLARIS_PROVIDER?.trim() || "codex",
        model: process.env.POLARIS_MODEL?.trim() || undefined,
      },
      { idempotencyKey: stableKey("batch-migration-session", job.id) },
    );
    checkpoints[job.id] = { sessionId: session.id, sequence: 0, status: "running" };
    await saveCheckpoints(checkpointPath, checkpoints);

    await session.sendTurn(
      {
        inputText: `${job.instruction}\n\nPreserve unrelated changes and verify the migration before reporting completion.`,
        runtimeMode: "full-access",
        interactionMode: "default",
      },
      { idempotencyKey: stableKey("batch-migration-turn", job.id) },
    );

    const terminal = await streamUntilTerminal(session, {
      afterSequence: checkpoints[job.id]?.sequence ?? 0,
      onEvent(event) {
        const text = outputText(event);
        if (text) process.stdout.write(`[${job.id}] ${text}`);
      },
      async onCheckpoint(sequence) {
        checkpoints[job.id] = { sessionId: session.id, sequence, status: "running" };
        await saveCheckpoints(checkpointPath, checkpoints);
      },
    });
    if (terminal.eventType !== "execution.completed") throw new Error(terminal.eventType);
    checkpoints[job.id] = {
      sessionId: session.id,
      sequence: terminal.sequence,
      status: "completed",
    };
    await saveCheckpoints(checkpointPath, checkpoints);
  } catch (error) {
    checkpoints[job.id] = {
      ...checkpoints[job.id],
      sequence: checkpoints[job.id]?.sequence ?? 0,
      status: "failed",
      error: error instanceof Error ? error.message : String(error),
    };
    await saveCheckpoints(checkpointPath, checkpoints);
  }
}

if (Object.values(checkpoints).some((checkpoint) => checkpoint.status === "failed")) {
  process.exitCode = 1;
}

function parseJobs(value: unknown): RepositoryJob[] {
  if (!Array.isArray(value) || value.length === 0)
    throw new Error("Input must be a non-empty JSON array.");
  return value.map((item, index) => {
    if (
      !isRecord(item) ||
      typeof item.id !== "string" ||
      typeof item.projectId !== "string" ||
      typeof item.instruction !== "string"
    ) {
      throw new Error(`Invalid repository job at index ${index}.`);
    }
    return { id: item.id, projectId: item.projectId, instruction: item.instruction };
  });
}

async function loadCheckpoints(path: string): Promise<Checkpoints> {
  try {
    const value = JSON.parse(await readFile(path, "utf8")) as unknown;
    return isRecord(value) ? (value as Checkpoints) : {};
  } catch (error) {
    if (isRecord(error) && error.code === "ENOENT") return {};
    throw error;
  }
}

async function saveCheckpoints(path: string, checkpoints: Checkpoints): Promise<void> {
  await writeFile(path, `${JSON.stringify(checkpoints, null, 2)}\n`, { mode: 0o600 });
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
