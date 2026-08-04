import { createHash } from "node:crypto";

import { Polaris, type SessionEvent, type SessionHandle } from "@polaris-agents/sdk";

export type ExampleEnvironment = {
  polaris: Polaris;
  projectId: string;
};

export function environment(): ExampleEnvironment {
  return {
    polaris: new Polaris({
      apiKey: requiredEnvironmentVariable("POLARIS_API_KEY"),
      baseUrl: requiredEnvironmentVariable("POLARIS_BASE_URL"),
    }),
    projectId: requiredEnvironmentVariable("POLARIS_PROJECT_ID"),
  };
}

export function requiredEnvironmentVariable(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) throw new Error(`${name} is required.`);
  return value;
}

export function stableKey(...parts: string[]): string {
  return createHash("sha256").update(parts.join("\0")).digest("hex");
}

export async function streamUntilTerminal(
  session: SessionHandle,
  options: {
    afterSequence?: number;
    onEvent?: (event: SessionEvent) => void | Promise<void>;
    onCheckpoint?: (sequence: number) => void | Promise<void>;
  } = {},
): Promise<SessionEvent> {
  const terminalTypes = new Set([
    "execution.completed",
    "execution.failed",
    "execution.cancelled",
    "execution.interrupted",
  ]);
  for await (const event of session.events({ afterSequence: options.afterSequence })) {
    await options.onEvent?.(event);
    await options.onCheckpoint?.(event.sequence);
    if (terminalTypes.has(event.eventType)) return event;
  }
  throw new Error("The Session event stream ended before an Execution reached a terminal state.");
}

export function outputText(event: SessionEvent): string | null {
  if (event.eventType !== "runtime.output.delta" && event.eventType !== "content.delta")
    return null;
  const value = event.payload.text ?? event.payload.delta;
  return typeof value === "string" ? value : null;
}
