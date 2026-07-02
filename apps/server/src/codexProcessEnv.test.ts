// FILE: codexProcessEnv.test.ts
// Purpose: Covers Codex account home-overlay auth isolation guarantees.
// Layer: Server utility tests.
// Exports: Vitest coverage for apps/server/src/codexProcessEnv.ts.
import {
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readlinkSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import OS from "node:os";
import path from "node:path";
import { afterEach, assert, describe, it } from "@effect/vitest";

import { buildCodexProcessEnv } from "./codexProcessEnv.ts";

describe("buildCodexProcessEnv account overlays", () => {
  const tempRoots: string[] = [];

  function makeTempRoot(): string {
    const root = mkdtempSync(path.join(OS.tmpdir(), "codex-process-env-test-"));
    tempRoots.push(root);
    return root;
  }

  afterEach(() => {
    while (tempRoots.length > 0) {
      const root = tempRoots.pop();
      if (root) {
        rmSync(root, { recursive: true, force: true });
      }
    }
  });

  function makeAccountFixture(input: { readonly shadowAuth: "real" | "symlink" | "missing" }) {
    const root = makeTempRoot();
    const homePath = path.join(root, "codex-home");
    const shadowHomePath = path.join(root, "codex-shadow-work");
    mkdirSync(homePath, { recursive: true });
    mkdirSync(shadowHomePath, { recursive: true });
    writeFileSync(path.join(homePath, "auth.json"), '{"account":"default"}', "utf8");
    writeFileSync(path.join(homePath, "config.toml"), "", "utf8");
    if (input.shadowAuth === "real") {
      writeFileSync(path.join(shadowHomePath, "auth.json"), '{"account":"work"}', "utf8");
    }
    if (input.shadowAuth === "symlink") {
      symlinkSync(path.join(homePath, "auth.json"), path.join(shadowHomePath, "auth.json"));
    }
    const env: NodeJS.ProcessEnv = {
      HOME: root,
      SYNARA_HOME: path.join(root, "synara-runtime"),
    };
    return { env, homePath, shadowHomePath };
  }

  it("links account-private auth from the shadow home instead of the shared home", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });

    const env = buildCodexProcessEnv({
      env: fixture.env,
      homePath: fixture.homePath,
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
      platform: "win32",
    });

    const overlayHomePath = env.CODEX_HOME;
    assert.ok(overlayHomePath);
    assert.notStrictEqual(path.resolve(overlayHomePath), path.resolve(fixture.homePath));
    const overlayAuthPath = path.join(overlayHomePath, "auth.json");
    assert.ok(lstatSync(overlayAuthPath).isSymbolicLink());
    assert.strictEqual(
      path.resolve(readlinkSync(overlayAuthPath)),
      path.resolve(path.join(fixture.shadowHomePath, "auth.json")),
    );
  });

  it("tolerates shadow homes with no auth state yet", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });

    const env = buildCodexProcessEnv({
      env: fixture.env,
      homePath: fixture.homePath,
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
      platform: "win32",
    });

    assert.ok(env.CODEX_HOME);
  });

  it("rejects shadow-home auth state that is itself a symlink", () => {
    const fixture = makeAccountFixture({ shadowAuth: "symlink" });

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          shadowHomePath: fixture.shadowHomePath,
          accountId: "work",
          platform: "win32",
        }),
      /is a symlink/,
    );
  });

  it("rejects a shadow home directory that is itself a symlink", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const aliasedShadowHome = path.join(path.dirname(fixture.shadowHomePath), "codex-shadow-alias");
    symlinkSync(fixture.homePath, aliasedShadowHome);

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: fixture.env,
          homePath: fixture.homePath,
          shadowHomePath: aliasedShadowHome,
          accountId: "work",
          platform: "win32",
        }),
      /shadow home/i,
    );
  });

  it("keeps shared auth out of account overlays without a shadow home", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const sharedEnv = { ...fixture.env, CODEX_HOME: fixture.homePath };

    const env = buildCodexProcessEnv({
      env: sharedEnv,
      accountId: "work",
      platform: "win32",
    });

    const overlayHomePath = env.CODEX_HOME;
    assert.ok(overlayHomePath);
    assert.notStrictEqual(path.resolve(overlayHomePath), path.resolve(fixture.homePath));
    assert.throws(() => lstatSync(path.join(overlayHomePath, "auth.json")));
  });

  it("keeps account-id-only instances isolated when the browser plugin is enabled", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const pluginEnabledEnv = {
      ...fixture.env,
      CODEX_HOME: fixture.homePath,
      DPCODE_DISABLE_CODEX_DPCODE_BROWSER_PLUGIN: "0",
    };
    writeFileSync(path.join(fixture.homePath, "config.toml"), 'model = "gpt-5.4"\n', "utf8");

    const env = buildCodexProcessEnv({
      env: pluginEnabledEnv,
      accountId: "work",
      platform: "win32",
    });

    const accountHomePath = env.CODEX_HOME;
    assert.ok(accountHomePath);
    assert.notStrictEqual(path.resolve(accountHomePath), path.resolve(fixture.homePath));
    assert.ok(lstatSync(accountHomePath).isDirectory());
    // The user's config must reach the account home unmodified (no forced
    // dpcode-browser disable), so plugin/model-provider settings apply.
    assert.strictEqual(
      readFileSync(path.join(accountHomePath, "config.toml"), "utf8"),
      'model = "gpt-5.4"\n',
    );
    // The default account keeps using the shared home in this mode.
    const defaultEnv = buildCodexProcessEnv({ env: pluginEnabledEnv, platform: "win32" });
    assert.strictEqual(defaultEnv.CODEX_HOME, fixture.homePath);
  });

  it("rejects a symlinked shadow home when the browser plugin is enabled", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const aliasedShadowHome = path.join(path.dirname(fixture.shadowHomePath), "codex-shadow-alias");
    symlinkSync(fixture.homePath, aliasedShadowHome);

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: {
            ...fixture.env,
            CODEX_HOME: fixture.homePath,
            DPCODE_DISABLE_CODEX_DPCODE_BROWSER_PLUGIN: "0",
          },
          shadowHomePath: aliasedShadowHome,
          accountId: "work",
          platform: "win32",
        }),
      /shadow home/i,
    );
  });

  it("rejects symlinked shadow auth state when the browser plugin is enabled", () => {
    const fixture = makeAccountFixture({ shadowAuth: "symlink" });

    assert.throws(
      () =>
        buildCodexProcessEnv({
          env: {
            ...fixture.env,
            CODEX_HOME: fixture.homePath,
            DPCODE_DISABLE_CODEX_DPCODE_BROWSER_PLUGIN: "0",
          },
          shadowHomePath: fixture.shadowHomePath,
          accountId: "work",
          platform: "win32",
        }),
      /is a symlink/,
    );
  });

  it("materializes the source config into direct shadow homes when the plugin is enabled", () => {
    const fixture = makeAccountFixture({ shadowAuth: "real" });
    writeFileSync(path.join(fixture.homePath, "config.toml"), 'model = "gpt-5.4"\n', "utf8");

    const env = buildCodexProcessEnv({
      env: {
        ...fixture.env,
        CODEX_HOME: fixture.homePath,
        DPCODE_DISABLE_CODEX_DPCODE_BROWSER_PLUGIN: "0",
      },
      shadowHomePath: fixture.shadowHomePath,
      accountId: "work",
      platform: "win32",
    });

    assert.strictEqual(env.CODEX_HOME, fixture.shadowHomePath);
    assert.strictEqual(
      readFileSync(path.join(fixture.shadowHomePath, "config.toml"), "utf8"),
      'model = "gpt-5.4"\n',
    );
    // The shadow home's own auth stays untouched.
    assert.strictEqual(
      readFileSync(path.join(fixture.shadowHomePath, "auth.json"), "utf8"),
      '{"account":"work"}',
    );
  });

  it("drops stale shared-auth symlinks when reusing the account home with the plugin enabled", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const pluginEnabledEnv = {
      ...fixture.env,
      CODEX_HOME: fixture.homePath,
      DPCODE_DISABLE_CODEX_DPCODE_BROWSER_PLUGIN: "0",
    };

    // Simulate an account home a previous overlay-mode build left behind:
    // auth.json symlinked to the shared home and a plugin-disabled config.
    const overlayEnv = buildCodexProcessEnv({
      env: { ...fixture.env, CODEX_HOME: fixture.homePath },
      accountId: "work",
      platform: "win32",
    });
    const accountHomePath = overlayEnv.CODEX_HOME;
    assert.ok(accountHomePath);
    symlinkSync(path.join(fixture.homePath, "auth.json"), path.join(accountHomePath, "auth.json"));

    const env = buildCodexProcessEnv({
      env: pluginEnabledEnv,
      accountId: "work",
      platform: "win32",
    });

    assert.strictEqual(env.CODEX_HOME, accountHomePath);
    // The stale alias to the default account's auth must be gone.
    assert.throws(() => lstatSync(path.join(accountHomePath, "auth.json")));
  });

  it("mirrors private auth from an account's own dedicated home", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });

    const env = buildCodexProcessEnv({
      env: fixture.env,
      homePath: fixture.homePath,
      accountId: "work",
      platform: "win32",
    });

    const overlayHomePath = env.CODEX_HOME;
    assert.ok(overlayHomePath);
    const overlayAuthPath = path.join(overlayHomePath, "auth.json");
    assert.ok(lstatSync(overlayAuthPath).isSymbolicLink());
    assert.strictEqual(
      path.resolve(readlinkSync(overlayAuthPath)),
      path.resolve(path.join(fixture.homePath, "auth.json")),
    );
  });

  it("drops legacy shared-auth aliases from account overlays and keeps own logins", () => {
    const fixture = makeAccountFixture({ shadowAuth: "missing" });
    const sharedEnv = { ...fixture.env, CODEX_HOME: fixture.homePath };

    const firstEnv = buildCodexProcessEnv({
      env: sharedEnv,
      accountId: "work",
      platform: "win32",
    });
    const overlayHomePath = firstEnv.CODEX_HOME;
    assert.ok(overlayHomePath);
    // Simulate the legacy overlay state that symlinked shared auth in.
    symlinkSync(path.join(fixture.homePath, "auth.json"), path.join(overlayHomePath, "auth.json"));

    const secondEnv = buildCodexProcessEnv({
      env: sharedEnv,
      accountId: "work",
      platform: "win32",
    });
    assert.strictEqual(secondEnv.CODEX_HOME, overlayHomePath);
    assert.throws(() => lstatSync(path.join(overlayHomePath, "auth.json")));

    // The account's own login is a real file and must survive re-preparation.
    writeFileSync(path.join(overlayHomePath, "auth.json"), '{"account":"work"}', "utf8");
    const thirdEnv = buildCodexProcessEnv({
      env: sharedEnv,
      accountId: "work",
      platform: "win32",
    });
    assert.strictEqual(thirdEnv.CODEX_HOME, overlayHomePath);
    assert.ok(lstatSync(path.join(overlayHomePath, "auth.json")).isFile());
  });
});
