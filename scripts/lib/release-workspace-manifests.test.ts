import * as FS from "node:fs";
import * as Path from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { RELEASE_WORKSPACE_MANIFEST_PATHS } from "./release-workspace-manifests";

const repoRoot = Path.resolve(Path.dirname(fileURLToPath(import.meta.url)), "../..");

function workspaceManifests(directory: "apps" | "packages"): string[] {
  return FS.readdirSync(Path.join(repoRoot, directory), { withFileTypes: true })
    .filter(
      (entry) =>
        entry.isDirectory() &&
        FS.existsSync(Path.join(repoRoot, directory, entry.name, "package.json")),
    )
    .map((entry) => `${directory}/${entry.name}/package.json`);
}

function readManifest(relativePath: string): Record<string, unknown> {
  return JSON.parse(FS.readFileSync(Path.join(repoRoot, relativePath), "utf8")) as Record<
    string,
    unknown
  >;
}

describe("release workspace staging", () => {
  it("copies every workspace importer represented by the repository lockfile", () => {
    const expected = [
      "package.json",
      ...workspaceManifests("apps"),
      ...workspaceManifests("packages"),
      "scripts/package.json",
    ].sort();

    expect([...RELEASE_WORKSPACE_MANIFEST_PATHS].sort()).toEqual(expected);
  });

  it("keeps the staged server runtime dependencies aligned with the pinned Effect revision", () => {
    const root = readManifest("package.json");
    const server = readManifest("apps/server/package.json");
    const catalog = (root.workspaces as { catalog: Record<string, string> }).catalog;
    const overrides = root.overrides as Record<string, string>;
    const serverDependencies = server.dependencies as Record<string, string>;

    expect(serverDependencies.ioredis).toBe("catalog:");
    expect(catalog.ioredis).toBe("^5.11.1");
    expect(overrides.effect).toBe(catalog.effect);
  });
});
