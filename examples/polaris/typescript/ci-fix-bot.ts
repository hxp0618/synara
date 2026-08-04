import { environment, outputText, stableKey, streamUntilTerminal } from "./common";

const jobId = process.env.CI_JOB_ID?.trim() || process.env.GITHUB_RUN_ID?.trim() || "local-ci-run";
const repository =
  process.env.CI_REPOSITORY?.trim() || process.env.GITHUB_REPOSITORY?.trim() || "repository";
const failureSummary =
  process.env.CI_FAILURE_SUMMARY?.trim() ||
  "The test job failed. Reproduce the failure, make the smallest safe repair, and verify it.";

const { polaris, projectId } = environment();
const session = await polaris.sessions.create(
  {
    projectId,
    title: `Repair CI for ${repository} (${jobId})`,
    provider: process.env.POLARIS_PROVIDER?.trim() || "codex",
    model: process.env.POLARIS_MODEL?.trim() || undefined,
  },
  { idempotencyKey: stableKey("ci-fix-session", repository, jobId) },
);

await session.sendTurn(
  {
    inputText: [
      `Repair the failed CI job ${jobId} in ${repository}.`,
      failureSummary,
      "Inspect the current repository state, preserve unrelated changes, run the focused failing test, then run the relevant quality gates.",
    ].join("\n\n"),
    runtimeMode: "full-access",
    interactionMode: "default",
  },
  { idempotencyKey: stableKey("ci-fix-turn", repository, jobId) },
);

const terminal = await streamUntilTerminal(session, {
  onEvent(event) {
    const text = outputText(event);
    if (text) process.stdout.write(text);
    if (
      event.eventType === "approval.requested" ||
      (event.eventType === "request.opened" &&
        typeof event.payload.requestType === "string" &&
        event.payload.requestType.endsWith("_approval"))
    ) {
      console.error("\nAgent is waiting for an approval; review it in the Polaris Console.");
    }
  },
});

if (terminal.eventType !== "execution.completed") {
  throw new Error(`CI repair ended with ${terminal.eventType}.`);
}
console.log(`\nCI repair Session completed: ${session.id}`);
