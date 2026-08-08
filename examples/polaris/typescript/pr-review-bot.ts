import { environment, outputText, stableKey, streamUntilTerminal } from "./common";

const repository = process.env.GITHUB_REPOSITORY?.trim() || "repository";
const pullRequest = process.env.PULL_REQUEST_NUMBER?.trim();
const headSha = process.env.PULL_REQUEST_HEAD_SHA?.trim();
if (!pullRequest || !headSha) {
  throw new Error("PULL_REQUEST_NUMBER and PULL_REQUEST_HEAD_SHA are required.");
}

const { polaris, projectId } = environment();
const session = await polaris.sessions.create(
  {
    projectId,
    title: `Review ${repository}#${pullRequest}`,
    provider: process.env.POLARIS_PROVIDER?.trim() || "codex",
    model: process.env.POLARIS_MODEL?.trim() || undefined,
  },
  { idempotencyKey: stableKey("pr-review-session", repository, pullRequest, headSha) },
);

await session.sendTurn(
  {
    inputText: [
      `Review pull request ${repository}#${pullRequest} at commit ${headSha}.`,
      "Prioritize correctness, security, data loss, concurrency, and missing tests.",
      "Do not modify the repository. Return only actionable findings with exact file and line evidence; say explicitly when there are no findings.",
    ].join("\n\n"),
    runtimeMode: "read-only",
    interactionMode: "default",
  },
  { idempotencyKey: stableKey("pr-review-turn", repository, pullRequest, headSha) },
);

let review = "";
const terminal = await streamUntilTerminal(session, {
  onEvent(event) {
    const text = outputText(event);
    if (text) review += text;
  },
});

if (terminal.eventType !== "execution.completed") {
  throw new Error(`Pull request review ended with ${terminal.eventType}.`);
}
process.stdout.write(
  review.trim() ? `${review.trim()}\n` : "No textual review output was produced.\n",
);
