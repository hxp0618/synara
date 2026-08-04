import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, describe, expect, it } from "vitest";

import {
  assertMatchingBetaVersions,
  preparePolarisSDKRelease,
  publicNPMManifest,
} from "./polaris-sdk-release";

const roots: string[] = [];
afterEach(async () => {
  await Promise.all(roots.splice(0).map((root) => rm(root, { recursive: true, force: true })));
});

describe("Polaris SDK release staging", () => {
  it("maps npm beta versions to PEP 440 and rejects mismatches", () => {
    expect(() => assertMatchingBetaVersions("0.1.0-beta.1", "0.1.0b1")).not.toThrow();
    expect(() => assertMatchingBetaVersions("0.1.0", "0.1.0")).toThrow(/semver beta/);
    expect(() => assertMatchingBetaVersions("0.1.0-beta.2", "0.1.0b1")).toThrow(/disagree/);
  });

  it("removes workspace-only package fields from the public manifest", () => {
    expect(
      publicNPMManifest({
        name: "@polaris-agents/sdk",
        version: "0.1.0-beta.1",
        private: true,
        scripts: { test: "vitest" },
        devDependencies: { vitest: "latest" },
        exports: { ".": "./dist/index.mjs" },
      }),
    ).toEqual({
      name: "@polaris-agents/sdk",
      version: "0.1.0-beta.1",
      exports: { ".": "./dist/index.mjs" },
      private: false,
    });
  });

  it("stages only built npm payloads and records their digests", async () => {
    const root = await mkdtemp(join(tmpdir(), "polaris-release-source-"));
    roots.push(root);
    const output = join(root, "output");
    const npm = join(root, "packages", "polaris-sdk");
    const python = join(root, "packages", "polaris-python");
    mkdirSync(join(npm, "dist"), { recursive: true });
    mkdirSync(join(python, "src", "polaris_agents"), { recursive: true });
    writeFileSync(
      join(npm, "package.json"),
      JSON.stringify({
        name: "@polaris-agents/sdk",
        version: "0.1.0-beta.1",
        private: true,
        repository: { url: "git+https://github.com/hxp0618/synara.git" },
        polaris: { minimumControlPlaneVersion: "0.1.0-beta.1" },
        files: ["dist", "README.md"],
      }),
    );
    for (const file of ["index.mjs", "index.cjs", "index.d.mts", "index.d.cts"])
      writeFileSync(join(npm, "dist", file), file);
    writeFileSync(join(npm, "README.md"), "npm readme");
    writeFileSync(
      join(python, "pyproject.toml"),
      '[project]\nname = "polaris-agents"\nversion = "0.1.0b1"\n\n[tool.polaris]\nminimum-control-plane-version = "0.1.0-beta.1"\n\n[tool.test]\nvalue = true\n',
    );
    writeFileSync(join(python, "README.md"), "python readme");
    writeFileSync(join(python, "src", "polaris_agents", "_generated.py"), "# generated");

    const result = preparePolarisSDKRelease(root, output);
    const staged = JSON.parse(readFileSync(join(result.npmDirectory, "package.json"), "utf8"));
    expect(staged.private).toBe(false);
    expect(staged.scripts).toBeUndefined();
    expect(result.sourceFiles.map((item) => item.path)).toContain("npm/dist/index.mjs");
    expect(result.sourceFiles.every((item) => /^sha256:[0-9a-f]{64}$/.test(item.sha256))).toBe(
      true,
    );
  });
});
