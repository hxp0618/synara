#!/usr/bin/env node
import { readFileSync } from "node:fs";

import {
  readRunnerCredential,
  runProviderHost,
  type RunnerInput,
  type RunnerMessage,
} from "./providerHost";
import { runProviderHostProtocolV2 } from "./protocol";

function emit(message: RunnerMessage): void {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

try {
  const credential = readRunnerCredential(process.env);
  if (process.argv.includes("--protocol-v2")) {
    await runProviderHostProtocolV2({
      source: process.stdin,
      credential,
      emit: (message) => process.stdout.write(`${JSON.stringify(message)}\n`),
    });
  } else {
    const encoded = readFileSync(0, "utf8");
    if (Buffer.byteLength(encoded) > 2 * 1024 * 1024) throw new Error("Runner input is too large");
    const input = JSON.parse(encoded) as RunnerInput;
    await runProviderHost(input, credential, emit);
  }
} catch (error) {
  const message = error instanceof Error ? error.message : String(error);
  process.stderr.write(`provider-host: ${message}\n`);
  process.exitCode = 1;
}
