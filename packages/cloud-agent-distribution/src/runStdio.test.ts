import { Readable, Writable } from "node:stream";

import { describe, expect, it } from "vitest";

import { CLOUD_AGENT_DISTRIBUTION_MANIFEST } from "./index";
import {
  CLOUD_AGENT_DISTRIBUTION_PROVIDER_ALLOWLIST,
  runDefaultCloudAgentRuntimeStdio,
} from "./runStdio";

describe("Cloud Agent Distribution registry stdio", () => {
  it("serves Describe through exactly the manifest allowlist", async () => {
    const output = capture();
    const diagnostics = capture();
    await runDefaultCloudAgentRuntimeStdio({
      source: Readable.from(`${JSON.stringify(describeCommand("codex"))}\n`),
      output: output.stream,
      diagnostics: diagnostics.stream,
    });
    expect(CLOUD_AGENT_DISTRIBUTION_PROVIDER_ALLOWLIST).toEqual(
      CLOUD_AGENT_DISTRIBUTION_MANIFEST.providers.map((provider) => provider.kind),
    );
    expect(JSON.parse(output.value())).toMatchObject({
      commandId: "describe-codex",
      messageType: "Result",
      payload: { descriptor: { providerKind: "codex" } },
    });
    expect(diagnostics.value()).toBe("");
  });

  it("fails closed for Providers outside the compiled manifest", async () => {
    const output = capture();
    await runDefaultCloudAgentRuntimeStdio({
      source: Readable.from(`${JSON.stringify(describeCommand("cursor"))}\n`),
      output: output.stream,
      diagnostics: capture().stream,
    });
    expect(JSON.parse(output.value())).toMatchObject({
      commandId: "describe-cursor",
      messageType: "Error",
      error: { code: "provider_not_installed" },
    });
  });
});

function describeCommand(provider: string) {
  return {
    requestId: `describe-${provider}`,
    protocolVersion: { major: 2, minor: 3 },
    executionId: "distribution-test",
    generation: 1,
    commandType: "Describe",
    commandId: `describe-${provider}`,
    occurredAt: "2026-08-09T00:00:00.000Z",
    payload: { provider },
  };
}

function capture(): { stream: Writable; value: () => string } {
  const chunks: Buffer[] = [];
  return {
    stream: new Writable({
      write(chunk, _encoding, callback) {
        chunks.push(Buffer.from(chunk));
        callback();
      },
    }),
    value: () => Buffer.concat(chunks).toString("utf8").trim(),
  };
}
