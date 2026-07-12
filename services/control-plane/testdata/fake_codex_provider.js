#!/usr/bin/env node

let input = "";
for await (const chunk of process.stdin) input += chunk;
if (!input.trim()) {
  process.stderr.write("fake Codex expected a prompt on stdin\n");
  process.exit(2);
}
const apiKey = process.env.OPENAI_API_KEY;
if (!apiKey) {
  process.stderr.write("fake Codex did not receive OPENAI_API_KEY\n");
  process.exit(3);
}
const reconstructed = input.includes("<synara_transcript>");
if (reconstructed) {
  if (
    process.argv.includes("resume") ||
    !input.includes("first durable question") ||
    !input.includes("first-turn-ok secret=[REDACTED]") ||
    !input.includes("second durable question")
  ) {
    process.stderr.write("fake Codex received an invalid reconstructed transcript\n");
    process.exit(4);
  }
}
const response = reconstructed ? "history-reconstructed-ok" : "first-turn-ok";
process.stdout.write(`${JSON.stringify({ type: "thread.started", thread_id: "docker-e2e-thread" })}\n`);
process.stdout.write(
  `${JSON.stringify({
    type: "item.completed",
    item: { type: "agent_message", text: `${response} secret=${apiKey}` },
  })}\n`,
);
process.stdout.write(
  `${JSON.stringify({
    type: "turn.completed",
    usage: { input_tokens: 1, output_tokens: 1 },
  })}\n`,
);
