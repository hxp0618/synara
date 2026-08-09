#!/usr/bin/env node
import {
  CODEX_TOOL_POLICY_HOOK_ARGUMENT,
  runCodexNoToolAwarePolicyHook,
} from "@synara/cloud-agent-provider-codex";

import { runDefaultCloudAgentRuntimeStdio } from "./runStdio";

try {
  if (process.argv.includes(CODEX_TOOL_POLICY_HOOK_ARGUMENT)) {
    await runCodexNoToolAwarePolicyHook();
  } else {
    await runDefaultCloudAgentRuntimeStdio();
  }
} catch (cause) {
  const message = cause instanceof Error ? cause.message : String(cause);
  process.stderr.write(`cloud-agent-runtime: ${message}\n`);
  process.exitCode = 1;
}
