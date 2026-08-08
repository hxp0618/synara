import { resolve } from "node:path";

import { preparePolarisSDKRelease } from "./lib/polaris-sdk-release";

const output = process.argv[2];
if (!output) {
  throw new Error("Usage: bun scripts/prepare-polaris-sdk-release.ts <new-output-directory>");
}

const result = preparePolarisSDKRelease(resolve(import.meta.dir, ".."), resolve(output));
console.log(
  `Prepared Polaris npm ${result.npmVersion} and Python ${result.pythonVersion} release inputs with ${result.sourceFiles.length} npm files.`,
);
