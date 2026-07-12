#!/usr/bin/env node

import fs from "node:fs/promises";
import path from "node:path";

import rootPackageJson from "../package.json" with { type: "json" };
import serverPackageJson from "../apps/server/package.json" with { type: "json" };
import { resolveCatalogDependencies } from "./lib/resolve-catalog.ts";

const outputDir = process.argv[2];
if (!outputDir) {
  throw new Error("Usage: prepare-server-runtime-package.ts <output-directory>");
}

const catalog = rootPackageJson.workspaces.catalog as Record<string, unknown>;
const dependencies = resolveCatalogDependencies(
  serverPackageJson.dependencies as Record<string, unknown>,
  catalog,
  "apps/server runtime dependencies",
);

const runtimeRootPackage = {
  name: "@synara/server-runtime",
  private: true,
  workspaces: ["apps/server"],
  packageManager: rootPackageJson.packageManager,
};

const runtimeServerPackage = {
  name: serverPackageJson.name,
  version: serverPackageJson.version,
  private: true,
  type: serverPackageJson.type,
  engines: serverPackageJson.engines,
  dependencies,
};

await fs.rm(outputDir, { recursive: true, force: true });
await fs.mkdir(path.join(outputDir, "apps/server"), { recursive: true });
await Promise.all([
  fs.writeFile(
    path.join(outputDir, "package.json"),
    `${JSON.stringify(runtimeRootPackage, null, 2)}\n`,
  ),
  fs.writeFile(
    path.join(outputDir, "apps/server/package.json"),
    `${JSON.stringify(runtimeServerPackage, null, 2)}\n`,
  ),
]);
