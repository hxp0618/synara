#!/usr/bin/env node
// FILE: verify-mac-distribution-environment.ts
// Purpose: Runs the non-secret macOS signed-distribution preflight for operators and CI.
// Layer: Release CLI

import { validateMacDistributionEnvironment } from "./lib/mac-distribution-preflight.ts";

try {
  const evidence = validateMacDistributionEnvironment({ environment: process.env });
  process.stdout.write(`${JSON.stringify(evidence, null, 2)}\n`);
} catch (error) {
  const message = error instanceof Error ? error.message : "Unknown macOS distribution error.";
  process.stderr.write(`macOS distribution preflight failed: ${message}\n`);
  process.exitCode = 1;
}
