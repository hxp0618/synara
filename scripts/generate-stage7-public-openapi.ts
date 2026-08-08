import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";

import { buildStage7PublicOpenAPI } from "./lib/stage7-public-openapi";

async function main(): Promise<void> {
  const sourcePath = resolve(process.argv[2] ?? "docs/api/openapi.yaml");
  const outputPath = resolve(process.argv[3] ?? "apps/developer-docs/.generated/openapi.yaml");
  const result = buildStage7PublicOpenAPI(await readFile(sourcePath, "utf8"));
  await mkdir(dirname(outputPath), { recursive: true });
  await writeFile(outputPath, result.document);
  console.log(
    `Generated ${result.operationIds.length} public operations: ${result.operationIds.join(", ")}`,
  );
}

if (import.meta.main) await main();
