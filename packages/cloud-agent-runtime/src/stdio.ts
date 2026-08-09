#!/usr/bin/env node
import { readFileSync } from "node:fs";

import {
  readRunnerCredential,
  runProviderHost,
  startProviderHostRun,
  type RunnerInput,
  type RunnerMessage,
} from "./providerHost";
import { runProviderHostProtocolV2 } from "./protocol";
import { createBoundedNdjsonWriter } from "./ndjsonWriter";
import { PROVIDER_HOST_MAX_MESSAGE_BYTES } from "@synara/contracts/provider-host";
import {
  CODEX_TOOL_POLICY_HOOK_ARGUMENT,
  buildCodexToolPolicyHookCommand,
  runCodexToolPolicyHook,
} from "./codexPostToolUseProvenance";

function emit(message: RunnerMessage): void {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

try {
  if (process.argv.includes(CODEX_TOOL_POLICY_HOOK_ARGUMENT)) {
    await runCodexToolPolicyHook();
  } else if (process.argv.includes("--protocol-v2")) {
    const credential = readRunnerCredential(process.env);
    const codexToolPolicyHookCommand = currentProviderHostToolPolicyHookCommand();
    const stdout = createBoundedNdjsonWriter({
      target: process.stdout,
      maximumMessageBytes: PROVIDER_HOST_MAX_MESSAGE_BYTES,
    });
    await runProviderHostProtocolV2({
      source: process.stdin,
      credential,
      emit: stdout.enqueue,
      flush: stdout.flush,
      startRun: (input, runnerCredential, runnerEmit, options) =>
        startProviderHostRun(input, runnerCredential, runnerEmit, {
          ...options,
          codexToolPolicyHookCommand,
        }),
    });
  } else {
    const credential = readRunnerCredential(process.env);
    const encoded = readFileSync(0, "utf8");
    if (Buffer.byteLength(encoded) > 2 * 1024 * 1024) throw new Error("Runner input is too large");
    const input = JSON.parse(encoded) as RunnerInput;
    await runProviderHost(input, credential, emit, {
      codexToolPolicyHookCommand: currentProviderHostToolPolicyHookCommand(),
    });
  }
} catch (error) {
  const message = error instanceof Error ? error.message : String(error);
  process.stderr.write(`provider-host: ${message}\n`);
  process.exitCode = 1;
}

function currentProviderHostToolPolicyHookCommand(): string {
  return buildCodexToolPolicyHookCommand({
    nodeExecutable: process.execPath,
    providerHostEntrypoint: process.argv[1] ?? "",
  });
}
